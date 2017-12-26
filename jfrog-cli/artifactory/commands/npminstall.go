package commands

import (
	"github.com/jfrogdev/jfrog-cli-go/jfrog-cli/artifactory/utils"
	"github.com/jfrogdev/jfrog-cli-go/jfrog-client/utils/log"
	"github.com/jfrogdev/jfrog-cli-go/jfrog-client/utils/errorutils"
	serviceutils "github.com/jfrogdev/jfrog-cli-go/jfrog-client/artifactory/services/utils"
	"os/exec"
	"errors"
	"os"
	"path/filepath"
	"io/ioutil"
	"encoding/json"
	"strings"
	"fmt"
	"github.com/jfrogdev/jfrog-cli-go/jfrog-cli/artifactory/utils/npm"
	"github.com/jfrogdev/jfrog-cli-go/jfrog-cli/artifactory/utils/buildinfo"
	"github.com/buger/jsonparser"
	"github.com/jfrogdev/gofrog/parallel"
	"github.com/jfrogdev/jfrog-cli-go/jfrog-client/artifactory"
	"github.com/jfrogdev/jfrog-cli-go/jfrog-client/artifactory/httpclient"
	cliutils "github.com/jfrogdev/jfrog-cli-go/jfrog-client/utils"
	"github.com/jfrogdev/jfrog-cli-go/jfrog-client/artifactory/services/utils/auth"
	"github.com/mattn/go-shellwords"
	"strconv"
	"github.com/jfrogdev/jfrog-cli-go/jfrog-cli/utils/ioutils"
)

const NPMRC_FILE_NAME = ".npmrc"
const NPMRC_BACKUP_FILE_NAME = "jfrog.npmrc.backup"

func NpmInstall(repo string, cliFlags *npm.CliFlags) (err error) {
	log.Info("Running npm Install.")
	npmi := npmInstall{flags: cliFlags}
	if err = npmi.preparePrerequisites(repo); err != nil {
		return err
	}

	if err = npmi.createTempNpmrc(); err != nil {
		return npmi.restoreNpmrcAndError(err)
	}

	if err = npmi.runInstall(); err != nil {
		return npmi.restoreNpmrcAndError(err)
	}

	if err = npmi.restoreNpmrc(); err != nil {
		return err
	}

	if !npmi.collectBuildInfo {
		log.Info("Npm install finished successfully.")
		return nil
	}

	if err = npmi.setDependenciesList(); err != nil {
		return err
	}

	if err = npmi.collectDependenciesChecksums(); err != nil {
		return err
	}

	if err = npmi.saveDependenciesData(); err != nil {
		return err
	}

	log.Info("Npm install finished successfully.")
	return
}

func (npmi *npmInstall) preparePrerequisites(repo string) error {
	log.Debug("Preparing prerequisites.")
	if err := npmi.setNpmExecutable(); err != nil {
		return err
	}

	if err := npmi.validateNpmVersion(); err != nil {
		return err
	}

	currentDir, err := os.Getwd()
	if err != nil {
		return errorutils.CheckError(err)
	}

	if currentDir, err = filepath.Abs(currentDir); err != nil {
		return errorutils.CheckError(err)
	}

	npmi.workingDirectory = currentDir
	log.Debug("Working directory set to:", npmi.workingDirectory)
	if err = npmi.setArtifactoryAuth(); err != nil {
		return errorutils.CheckError(err)
	}

	npmAuth, err := getNpmAuth(npmi.artDetails)
	if err != nil {
		return err
	}

	if len(npmi.flags.BuildName) > 0 && len(npmi.flags.BuildNumber) > 0 {
		npmi.collectBuildInfo = true
		if err := utils.SaveBuildGeneralDetails(npmi.flags.BuildName, npmi.flags.BuildNumber); err != nil {
			return err
		}
	}

	npmi.npmAuth = string(npmAuth)
	npmi.registry = getNpmRepositoryUrl(repo, npmi.artDetails.Url)
	return npmi.backupProjectNpmrc()
}

// In order to make sure the install downloads the dependencies from Artifactory, we are creating a.npmrc file in the project's root directory.
// If such a file already exists, we are copying it aside.
// This method restores the backed up file and deletes the one created by the command.
func (npmi *npmInstall) restoreNpmrc() (err error) {
	log.Debug("Restoring project .npmrc file")
	if err = os.Remove(filepath.Join(npmi.workingDirectory, NPMRC_FILE_NAME)); err != nil {
		return errorutils.CheckError(errors.New(createRestoreErrorPrefix(npmi.workingDirectory) + err.Error()))
	}
	log.Debug("Deleted the temporary .npmrc file successfully")

	if _, err = os.Stat(filepath.Join(npmi.workingDirectory, NPMRC_BACKUP_FILE_NAME)); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errorutils.CheckError(errors.New(createRestoreErrorPrefix(npmi.workingDirectory) + err.Error()))
	}

	if err = ioutils.CopyFile(
		filepath.Join(npmi.workingDirectory, NPMRC_BACKUP_FILE_NAME),
		filepath.Join(npmi.workingDirectory, NPMRC_FILE_NAME), npmi.npmrcFileMode); err != nil {
		return errorutils.CheckError(err)
	}
	log.Debug("Restored project .npmrc file successfully")

	if err = os.Remove(filepath.Join(npmi.workingDirectory, NPMRC_BACKUP_FILE_NAME)); err != nil {
		return errorutils.CheckError(errors.New(createRestoreErrorPrefix(npmi.workingDirectory) + err.Error()))
	}
	log.Debug("Deleted project", NPMRC_BACKUP_FILE_NAME, "file successfully")
	return nil
}

func createRestoreErrorPrefix(workingDirectory string) string {
	return fmt.Sprintf("Error occurred while restoring project .npmrc file. "+
		"Delete '%s' and move '%s' (if exists) to '%s' in order to restore the project. Failure cause: \n",
		filepath.Join(workingDirectory, NPMRC_FILE_NAME),
		filepath.Join(workingDirectory, NPMRC_BACKUP_FILE_NAME),
		filepath.Join(workingDirectory, NPMRC_FILE_NAME))
}

// In order to make sure the install downloads the artifacts from Artifactory we creating in the project .npmrc file.
// If such a file exists we storing a copy of it in NPMRC_BACKUP_FILE_NAME.
func (npmi *npmInstall) createTempNpmrc() error {
	log.Debug("Creating project .npmrc file.")
	data, err := npm.GetConfigList(npmi.flags.NpmArgs, npmi.executablePath)
	configData, err := npmi.prepareConfigData(data)
	if err != nil {
		return errorutils.CheckError(err)
	}

	if err = removeNpmrcIfExists(npmi.workingDirectory); err != nil {
		return err
	}

	return errorutils.CheckError(ioutil.WriteFile(filepath.Join(npmi.workingDirectory, NPMRC_FILE_NAME), configData, npmi.npmrcFileMode))
}

func (npmi *npmInstall) runInstall() error {
	log.Debug("Running npmi install command.")
	splitArgs, err := shellwords.Parse(npmi.flags.NpmArgs)
	if err != nil {
		return errorutils.CheckError(err)
	}
	filteredArgs := filterFlags(splitArgs)
	installCmdConfig := &npm.NpmConfig{
		Npm:          npmi.executablePath,
		Command:      append([]string{"install"}, filteredArgs...),
		CommandFlags: nil,
		StrWriter:    nil,
		ErrWriter:    nil,
	}

	if npmi.collectBuildInfo && len(filteredArgs) > 0 {
		log.Warn("Build info dependencies collection with npm arguments is not supported. Build info creation will be skipped.")
		npmi.collectBuildInfo = false
	}

	return errorutils.CheckError(utils.RunCmd(installCmdConfig))
}

func (npmi *npmInstall) setDependenciesList() (err error) {
	npmi.dependencies = make(map[string]*dependency)
	// npmi.scope can be empty, "production" or "development" in case of empty both of the functions should run
	if npmi.typeRestriction != "production" {
		if err = npmi.prepareDependencies("development"); err != nil {
			return
		}
	}
	if npmi.typeRestriction != "development" {
		err = npmi.prepareDependencies("production")
	}
	return
}

func (npmi *npmInstall) collectDependenciesChecksums() error {
	log.Debug("Collecting dependencies checksums.")
	servicesManager, err := utils.CreateServiceManager(npmi.flags.ArtDetails, false)
	if err != nil {
		return err
	}

	producerConsumer := parallel.NewBounedRunner(3, false)
	errorsQueue := serviceutils.NewErrorsQueue(1)
	handlerFunc := npmi.createGetDependencyInfoFunc(servicesManager)
	go func() {
		defer producerConsumer.Done()
		for i := range npmi.dependencies {
			producerConsumer.AddTaskWithError(handlerFunc(i), errorsQueue.AddError)
		}
	}()
	producerConsumer.Run()
	return errorsQueue.GetError()
}

func (npmi *npmInstall) saveDependenciesData() error {
	log.Debug("Saving install data.")
	dependencies, missingDependencies := npmi.transformDependencies()
	populateFunc := func(partial *buildinfo.Partial) {
		partial.Dependencies = dependencies
	}

	if err := utils.SavePartialBuildInfo(npmi.flags.BuildName, npmi.flags.BuildNumber, populateFunc); err != nil {
		return err
	}

	if len(missingDependencies) > 0 {
		var missingDependenciesText []string
		for _, dependency := range missingDependencies {
			missingDependenciesText = append(missingDependenciesText, dependency.name+"-"+dependency.version)
		}
		log.Warn("Some npm dependencies could not be found in Artifactory and therefore are not included in the build-info. " +
			"You can fix this, by moving aside your project's node_modules and also your npm cache directory and then run this command again. " +
			"This will force npm to download all dependencies from Artifactory. Future builds will not need to download these dependencies again. " +
			"Here are the missing dependencies:\n" + strings.Join(missingDependenciesText, "\n"))
	}
	return nil
}

func (npmi *npmInstall) validateNpmVersion() error {
	version, err := npm.Version(npmi.executablePath)
	if err != nil {
		return err
	}
	versionParts := strings.Split(string(version), ".")
	versionInt, err := strconv.ParseInt(versionParts[0]+versionParts[1], 10, 32)
	if err != nil {
		return err
	}

	if versionInt < 54 {
		return errorutils.CheckError(errors.New("JFrog cli npm-install command requires npm client version 5.4.0 or higher."))
	}
	return nil
}

// To make npm do the resolution from Artifactory we are creating .npmrc file in the project dir.
// If a .npmrc file already exists we will backup it and override while running the command
func (npmi *npmInstall) backupProjectNpmrc() error {
	fileInfo, err := os.Stat(filepath.Join(npmi.workingDirectory, NPMRC_FILE_NAME))
	if err != nil {
		if os.IsNotExist(err) {
			npmi.npmrcFileMode = 0644
			return nil
		}
		return errorutils.CheckError(err)
	}

	npmi.npmrcFileMode = fileInfo.Mode()
	if err = ioutils.CopyFile(
		filepath.Join(npmi.workingDirectory, NPMRC_FILE_NAME),
		filepath.Join(npmi.workingDirectory, NPMRC_BACKUP_FILE_NAME), npmi.npmrcFileMode); err != nil {
		return err
	}
	log.Debug("Project .npmrc file backed up successfully to", filepath.Join(npmi.workingDirectory, NPMRC_BACKUP_FILE_NAME))
	return nil
}

// This func transforms "npm config list --json" result to key=val list of values that can be set to .npmrc file.
// it filters any nil values key, changes registry and scope registries to Artifactory url and adds Artifactory authentication to the list
func (npmi *npmInstall) prepareConfigData(data []byte) ([]byte, error) {
	var collectedConfig map[string]interface{}
	var filteredConf []string
	if err := json.Unmarshal(data, &collectedConfig); err != nil {
		return nil, errorutils.CheckError(err)
	}

	for i := range collectedConfig {
		if isValidKeyVal(i, collectedConfig[i]) {
			filteredConf = append(filteredConf, i, " = ", fmt.Sprint(collectedConfig[i]), "\n")
		} else if strings.HasPrefix(i, "@") {
			// Override scoped registries (@scope = xyz)
			filteredConf = append(filteredConf, i, " = ", npmi.registry, "\n")
		}
		npmi.setTypeRestriction(i, collectedConfig[i])
	}
	filteredConf = append(filteredConf, "registry = ", npmi.registry, "\n")
	filteredConf = append(filteredConf, npmi.npmAuth)
	return []byte(strings.Join(filteredConf, "")), nil
}

// npm install type restriction can be set by "--production" or "-only={prod[uction]|dev[elopment]}" flags
func (npmi *npmInstall) setTypeRestriction(key string, val interface{}) {
	if key == "production" && val != nil && (val == true || val == "true") {
		npmi.typeRestriction = "production"
	} else if key == "only" && val != nil {
		if strings.Contains(val.(string), "prod") {
			npmi.typeRestriction = "production"
		} else if strings.Contains(val.(string), "dev") {
			npmi.typeRestriction = "development"
		}
	}
}

// Run npm list and parse the returned json
func (npmi *npmInstall) prepareDependencies(typeRestriction string) error {
	// Run npm list
	data, errData, err := npm.RunList(npmi.flags.NpmArgs+" -only="+typeRestriction, npmi.executablePath)
	if err != nil {
		return err
	}
	if len(errData) > 0 {
		log.Warn("Some errors occurred while collecting dependencies info:\n" + string(errData))
	}

	// Parse the dependencies json object
	return jsonparser.ObjectEach(data, func(key []byte, value []byte, dataType jsonparser.ValueType, offset int) error {
		if string(key) == "dependencies" {
			err := npmi.parseDependencies(value, typeRestriction)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// Parses npm dependencies recursively and adds the collected dependencies to npmi.dependencies
func (npmi *npmInstall) parseDependencies(data []byte, scope string) error {
	var transitiveDependencies [][]byte
	err := jsonparser.ObjectEach(data, func(key []byte, value []byte, dataType jsonparser.ValueType, offset int) error {
		version, _, _, err := jsonparser.Get(data, string(key), "version")
		if err != nil {
			return errorutils.CheckError(err)
		}

		dependencyKey := string(key) + "-" + string(version)
		if npmi.dependencies[dependencyKey] == nil {
			npmi.dependencies[dependencyKey] = &dependency{name: string(key), version: string(version), scopes: []string{scope}}
		} else if !scopeAlreadyExists(scope, npmi.dependencies[dependencyKey].scopes) {
			npmi.dependencies[dependencyKey].scopes = append(npmi.dependencies[dependencyKey].scopes, scope)
		}
		transitive, _, _, err := jsonparser.Get(data, string(key), "dependencies")
		if err != nil && err.Error() != "Key path not found" {
			return errorutils.CheckError(err)
		}

		if len(transitive) > 0 {
			transitiveDependencies = append(transitiveDependencies, transitive)
		}
		return nil
	})

	if err != nil {
		return err
	}

	for _, element := range transitiveDependencies {
		err := npmi.parseDependencies(element, scope)
		if err != nil {
			return err
		}
	}
	return nil
}

// Creates a function that fetches dependency data from Artifactory. Can be applied from a producer-consumer mechanism
func (npmi *npmInstall) createGetDependencyInfoFunc(servicesManager *artifactory.ArtifactoryServicesManager) getDependencyInfoFunc {
	return func(dependencyIndex string) parallel.TaskFunc {
		return func(threadId int) error {
			threadPrefix := fmt.Sprintf("[Thread - %s ]", threadId)
			name := npmi.dependencies[dependencyIndex].name
			version := npmi.dependencies[dependencyIndex].version
			log.Debug(threadPrefix, "Fetching checksums for", name, "-", version)
			result, err := servicesManager.Aql(serviceutils.CreateAqlQueryForNpm(name, version))
			if err != nil {
				return err
			}

			parsedResult := new(aqlResult)
			if err = json.Unmarshal(result, parsedResult); err != nil {
				return errorutils.CheckError(err)
			}
			if len(parsedResult.Results) == 0 {
				log.Debug(threadPrefix, name, "-", version, "could not be found in Artifactory.")
				return nil
			}
			npmi.dependencies[dependencyIndex].artifactName = parsedResult.Results[0].Name
			npmi.dependencies[dependencyIndex].checksum =
				&buildinfo.Checksum{Sha1: parsedResult.Results[0].Actual_sha1, Md5: parsedResult.Results[0].Actual_md5}
			log.Debug(threadPrefix, "Found", parsedResult.Results[0].Name,
				"sha1:", parsedResult.Results[0].Actual_sha1,
				"md5", parsedResult.Results[0].Actual_md5)
			return nil
		}
	}
}

// Transforms the list of dependencies to buildinfo.Dependencies list and creates a list of dependencies that are missing in Artifactory.
func (npmi *npmInstall) transformDependencies() (dependencies []buildinfo.Dependencies, missingDependencies []dependency) {
	for _, dependency := range npmi.dependencies {
		if dependency.artifactName != "" {
			dependencies = append(dependencies,
				buildinfo.Dependencies{Id: dependency.artifactName, Scopes: dependency.scopes, Checksum: dependency.checksum})
		} else {
			missingDependencies = append(missingDependencies, *dependency)
		}
	}
	return
}

func (npmi *npmInstall) restoreNpmrcAndError(err error) error {
	if restoreErr := npmi.restoreNpmrc(); restoreErr != nil {
		return errors.New(fmt.Sprintf("Two errors occurred:\n %s\n %s", restoreErr.Error(), err.Error()))
	}
	return err
}

func (npmi *npmInstall) setArtifactoryAuth() error {
	authArtDetails, err := npmi.flags.ArtDetails.CreateArtAuthConfig()
	if err != nil {
		return err
	}
	if authArtDetails.SshAuthHeaders != nil {
		return errorutils.CheckError(errors.New("SSH authentication is not supported in this command."))
	}
	npmi.artDetails = authArtDetails
	return nil
}

func removeNpmrcIfExists(workingDirectory string) error {
	if _, err := os.Stat(filepath.Join(workingDirectory, NPMRC_FILE_NAME)); err != nil {
		if os.IsNotExist(err) { // The file dose not exist, nothing to do.
			return nil
		}
		return errorutils.CheckError(err)
	}

	log.Debug("Removing Existing .npmrc file")
	return errorutils.CheckError(os.Remove(filepath.Join(workingDirectory, NPMRC_FILE_NAME)))
}

func (npmi *npmInstall) setNpmExecutable() error {
	npmExecPath, err := exec.LookPath("npm")
	if err != nil {
		return errorutils.CheckError(err)
	}

	if npmExecPath == "" {
		return errorutils.CheckError(errors.New("Could not find 'npm' executable"))
	}
	npmi.executablePath = npmExecPath
	log.Debug("Found npm executable at:", npmi.executablePath)
	return nil
}

func getNpmAuth(artDetails *auth.ArtifactoryDetails) ([]byte, error) {
	authApiUrl := artDetails.Url + "api/npm/auth"
	log.Debug("Sending npm auth request")
	client := httpclient.NewDefaultHttpClient()
	resp, body, _, err := client.SendGet(authApiUrl, true, artDetails.CreateArtifactoryHttpClientDetails())
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, errorutils.CheckError(errors.New("Artifactory response: " + resp.Status + "\n" + cliutils.IndentJson(body)))
	}
	return body, err
}

func getNpmRepositoryUrl(repo, url string) string {
	if !strings.HasSuffix(url, "/") {
		url += "/"
	}
	url += "api/npm/" + repo
	return url
}

func scopeAlreadyExists(scope string, existingScopes []string) bool {
	for _, existingScope := range existingScopes {
		if existingScope == scope {
			return true
		}
	}
	return false
}

// Valid configs keys are not related to registry (registry = xyz) or scoped registry (@scope = xyz)) and have data in their value
func isValidKeyVal(key string, val interface{}) bool {
	return !strings.HasPrefix(key, "//") &&
		!strings.HasPrefix(key, "@") &&
		key != "registry" &&
		key != "metrics-registry" &&
		val != nil &&
		val != ""
}

func filterFlags(splitArgs []string) []string {
	var filteredArgs []string
	for _, arg := range splitArgs {
		if !strings.HasPrefix(arg, "-") {
			filteredArgs = append(filteredArgs, arg)
		}
	}
	return filteredArgs
}

type getDependencyInfoFunc func(string) parallel.TaskFunc

type npmInstall struct {
	executablePath   string
	flags            *npm.CliFlags
	npmrcFileMode    os.FileMode
	workingDirectory string
	registry         string
	npmAuth          string
	collectBuildInfo bool
	dependencies     map[string]*dependency
	typeRestriction  string
	artDetails       *auth.ArtifactoryDetails
}

type dependency struct {
	name         string
	version      string
	scopes       []string
	artifactName string
	checksum     *buildinfo.Checksum
}

type aqlResult struct {
	Results []*results `json:"results,omitempty"`
}

type results struct {
	Name        string `json:"name,omitempty"`
	Actual_md5  string `json:"actual_md5,omitempty"`
	Actual_sha1 string `json:"actual_sha1,omitempty"`
}
