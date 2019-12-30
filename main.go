package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bitrise-io/go-android/cache"
	"github.com/bitrise-io/go-android/gradle"
	"github.com/bitrise-io/go-steputils/stepconf"
	"github.com/bitrise-io/go-utils/command"
	"github.com/bitrise-io/go-utils/log"
	"github.com/bitrise-io/go-utils/pathutil"
	"github.com/bitrise-io/go-utils/sliceutil"
	"github.com/bitrise-steplib/bitrise-step-android-unit-test/testaddon"
	shellquote "github.com/kballard/go-shellquote"
)

// Configs ...
type Configs struct {
	ProjectLocation      string `env:"project_location,dir"`
	HTMLResultDirPattern string `env:"report_path_pattern"`
	XMLResultDirPattern  string `env:"result_path_pattern"`
	Variant              string `env:"variant"`
	Module               string `env:"module"`
	Arguments            string `env:"arguments"`
	CacheLevel           string `env:"cache_level,opt[none,only_deps,all]"`
	IsDebug              bool   `env:"is_debug,opt[true,false]"`

	DeployDir     string `env:"BITRISE_DEPLOY_DIR"`
	TestResultDir string `env:"BITRISE_TEST_RESULT_DIR"`
}

func failf(f string, args ...interface{}) {
	log.Errorf(f, args...)
	os.Exit(1)
}

func getArtifacts(gradleProject gradle.Project, started time.Time, pattern string, includeModuleName bool, isDirectoryMode bool) (artifacts []gradle.Artifact, err error) {
	for _, t := range []time.Time{started, time.Time{}} {
		if isDirectoryMode {
			artifacts, err = gradleProject.FindDirs(t, pattern, includeModuleName)
		} else {
			artifacts, err = gradleProject.FindArtifacts(t, pattern, includeModuleName)
		}
		if err != nil {
			return
		}
		if len(artifacts) == 0 {
			if t == started {
				log.Warnf("No artifacts found with pattern: %s that has modification time after: %s", pattern, t)
				log.Warnf("Retrying without modtime check....")
				fmt.Println()
				continue
			}
			log.Warnf("No artifacts found with pattern: %s without modtime check", pattern)
			log.Warnf("If you have changed default report export path in your gradle files then you might need to change ReportPathPattern accordingly.")
		}
	}
	return
}

func workDirRel(pth string) (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Rel(wd, pth)
}

func exportArtifacts(deployDir string, artifacts []gradle.Artifact) error {
	for _, artifact := range artifacts {
		artifact.Name += ".zip"
		exists, err := pathutil.IsPathExists(filepath.Join(deployDir, artifact.Name))
		if err != nil {
			return fmt.Errorf("failed to check path, error: %v", err)
		}

		if exists {
			timestamp := time.Now().Format("20060102150405")
			artifact.Name = fmt.Sprintf("%s-%s%s", strings.TrimSuffix(artifact.Name, ".zip"), timestamp, ".zip")
		}

		src := filepath.Base(artifact.Path)
		if rel, err := workDirRel(artifact.Path); err == nil {
			src = "./" + rel
		}

		log.Printf("  Export [ %s => $BITRISE_DEPLOY_DIR/%s ]", src, artifact.Name)

		if err := artifact.ExportZIP(deployDir); err != nil {
			log.Warnf("failed to export artifact (%s), error: %v", artifact.Path, err)
			continue
		}
	}
	return nil
}

func filterVariants(module, variant string, variantsMap gradle.Variants) (gradle.Variants, error) {
	// if module set: drop all the other modules
	if module != "" {
		v, ok := variantsMap[module]
		if !ok {
			return nil, fmt.Errorf("module not found: %s", module)
		}
		variantsMap = gradle.Variants{module: v}
	}
	// if variant not set: use all variants
	if variant == "" {
		return variantsMap, nil
	}
	filteredVariants := gradle.Variants{}
	for m, variants := range variantsMap {
		for _, v := range variants {
			if strings.ToLower(v) == strings.ToLower(variant+"UnitTest") {
				filteredVariants[m] = append(filteredVariants[m], v)
			}
		}
	}
	if len(filteredVariants) == 0 {
		return nil, fmt.Errorf("variant: %s not found in any module", variant)
	}
	return filteredVariants, nil
}

func tryExportTestAddonArtifact(artifactPth, outputDir string, lastOtherDirIdx int) int {
	dir := getExportDir(artifactPth)

	if dir == OtherDirName {
		// start indexing other dir name, to avoid overrideing it
		// e.g.: other, other-1, other-2
		lastOtherDirIdx++
		if lastOtherDirIdx > 0 {
			dir = dir + "-" + strconv.Itoa(lastOtherDirIdx)
		}
	}

	if err := testaddon.ExportArtifact(artifactPth, outputDir, dir); err != nil {
		log.Warnf("Failed to export test results for test addon: %s", err)
	} else {
		src := artifactPth
		if rel, err := workDirRel(artifactPth); err == nil {
			src = "./" + rel
		}
		log.Printf("  Export [%s => %s]", src, filepath.Join("$BITRISE_TEST_RESULT_DIR", dir, filepath.Base(artifactPth)))
	}
	return lastOtherDirIdx
}

// GetVariants ...
func (task *Task) GetAllVariants() (Variants, error) {
	tasksOutput, err := getGradleOutput(task.project.location, "tasks", "--all", "--console=plain", "--quiet")
	if err != nil {
		return nil, fmt.Errorf("%s, %s", tasksOutput, err)
	}
	return task.parseAllVariants(tasksOutput), nil
}

func (task *Task) parseAllVariants(gradleOutput string) Variants {
	//example gradleOutput:
	//"
	// lintMyflavorokStaging - Runs lint on the MyflavorokStaging build.
	// lintMyflavorRelease - Runs lint on the MyflavorRelease build.
	// lintVitalMyflavorRelease - Runs lint on the MyflavorRelease build.
	// lintMyflavorStaging - Runs lint on the MyflavorStaging build."
	tasks := Variants{}
lines:
	for _, l := range strings.Split(gradleOutput, "\n") {
		// l: " lintMyflavorokStaging - Runs lint on the MyflavorokStaging build."
		l = strings.TrimSpace(l)
		// l: "lintMyflavorokStaging - Runs lint on the MyflavorokStaging build."
		if l == "" {
			continue
		}
		// l: "lintMyflavorokStaging"
		l = strings.Split(l, " ")[0]
		var module string

		log.Warnf("Gradle Tasks: %s", l)
		split := strings.Split(l, ":")
		log.Warnf("Variant module split: %s", split)
		size := len(split)

		log.Warnf("Variant module size: %s", size)
		if size > 1 {
			module = strings.Join(split[:size - 1], ":")
			l = split[size - 1]
		}
		// module removed if any
		if strings.HasPrefix(l, task.name) {
			// task.name: "lint"
			// strings.HasPrefix will match lint and lintVital prefix also, we won't need lintVital so it is a conflict
			for _, conflict := range conflicts[task.name] {
				if strings.HasPrefix(l, conflict) {
					// if line has conflicting prefix don't do further checks with this line, skip...
					continue lines
				}
			}
			l = strings.TrimPrefix(l, task.name)
			// l: "MyflavorokStaging"
			if l == "" {
				continue
			}

			tasks[module] = append(tasks[module], l)
		}
	}

	for module, variants := range tasks {
		tasks[module] = cleanStringSlice(variants)
	}

	return tasks
}

func main() {
	var config Configs

	if err := stepconf.Parse(&config); err != nil {
		failf("Couldn't create step config: %v\n", err)
	}

	stepconf.Print(config)
	fmt.Println()

	log.SetEnableDebugLog(config.IsDebug)

	gradleProject, err := gradle.NewProject(config.ProjectLocation)
	if err != nil {
		failf("Failed to open project, error: %s", err)
	}

	testTask := gradleProject.GetTask("test")

	log.Infof("Variants:")
	fmt.Println()

	variants, err := testTask.GetAllVariants()
	if err != nil {
		failf("Failed to fetch variants, error: %s", err)
	}

	filteredVariants, err := filterVariants(config.Module, config.Variant, variants)
	if err != nil {
		failf("Failed to find buildable variants, error: %s", err)
	}

	for module, variants := range variants {
		log.Printf("%s:", module)
		for _, variant := range variants {
			if sliceutil.IsStringInSlice(variant, filteredVariants[module]) {
				log.Donef("✓ %s", strings.TrimSuffix(variant, "UnitTest"))
			} else {
				log.Printf("- %s", strings.TrimSuffix(variant, "UnitTest"))
			}
		}
	}
	fmt.Println()

	started := time.Now()

	args, err := shellquote.Split(config.Arguments)
	if err != nil {
		failf("Failed to parse arguments, error: %s", err)
	}

	var testErr error

	log.Infof("Run test:")
	testCommand := testTask.GetCommand(filteredVariants, args...)

	fmt.Println()
	log.Donef("$ " + testCommand.PrintableCommandArgs())
	fmt.Println()

	testErr = testCommand.Run()
	if testErr != nil {
		log.Errorf("Test task failed, error: %v", testErr)
	}
	fmt.Println()
	log.Infof("Export HTML results:")
	fmt.Println()

	reports, err := getArtifacts(gradleProject, started, config.HTMLResultDirPattern, true, true)
	if err != nil {
		failf("Failed to find reports, error: %v", err)
	}

	if err := exportArtifacts(config.DeployDir, reports); err != nil {
		failf("Failed to export reports, error: %v", err)
	}

	fmt.Println()
	log.Infof("Export XML results:")
	fmt.Println()

	results, err := getArtifacts(gradleProject, started, config.XMLResultDirPattern, true, true)
	if err != nil {
		failf("Failed to find results, error: %v", err)
	}

	if err := exportArtifacts(config.DeployDir, results); err != nil {
		failf("Failed to export results, error: %v", err)
	}

	if config.TestResultDir != "" {
		// Test Addon is turned on
		fmt.Println()
		log.Infof("Export XML results for test addon:")
		fmt.Println()

		xmlResultFilePattern := config.XMLResultDirPattern
		if !strings.HasSuffix(xmlResultFilePattern, "*.xml") {
			xmlResultFilePattern += "*.xml"
		}

		resultXMLs, err := getArtifacts(gradleProject, started, xmlResultFilePattern, false, false)
		if err != nil {
			log.Warnf("Failed to find test XML test results, error: %s", err)
		} else {
			lastOtherDirIdx := -1
			for _, artifact := range resultXMLs {
				lastOtherDirIdx = tryExportTestAddonArtifact(artifact.Path, config.TestResultDir, lastOtherDirIdx)
			}
		}
	}

	if testErr != nil {
		os.Exit(1)
	}

	fmt.Println()
	log.Infof("Collecting cache:")
	if warning := cache.Collect(config.ProjectLocation, cache.Level(config.CacheLevel)); warning != nil {
		log.Warnf("%s", warning)
	}
	log.Donef("  Done")
}
