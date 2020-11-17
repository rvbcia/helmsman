package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"gopkg.in/yaml.v2"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hashicorp/go-version"

	"github.com/Praqma/helmsman/internal/gcs"
)

type helmRepo struct {
	Name string `json:"name"`
	Url  string `json:"url"`
}

// helmCmd prepares a helm command to be executed
func helmCmd(args []string, desc string) Command {
	return Command{
		Cmd:         helmBin,
		Args:        args,
		Description: desc,
	}
}

var versionExtractor = regexp.MustCompile(`[\n]version:\s?(.*)`)

// validateChart validates if chart with the same name and version as specified in the DSF exists
func validateChart(apps, chart, version string, c chan string) {
	if isLocalChart(chart) {
		cmd := helmCmd([]string{"inspect", "chart", chart}, "Validating [ "+chart+" ] chart's availability")

		result := cmd.Exec()
		if result.code != 0 {
			maybeRepo := filepath.Base(filepath.Dir(chart))
			c <- "Chart [ " + chart + " ] for apps [" + apps + "] can't be found. Inspection returned error: \"" +
				strings.TrimSpace(result.errors) + "\" -- If this is not a local chart, add the repo [ " + maybeRepo + " ] in your helmRepos stanza."
			return
		}
		matches := versionExtractor.FindStringSubmatch(result.output)
		if len(matches) == 2 {
			v := strings.Trim(matches[1], `'"`)
			if strings.Trim(version, `'"`) != v {
				c <- "Chart [ " + chart + " ] with version [ " + version + " ] is specified for " +
					"apps [" + apps + "] but the chart found at that path has version [ " + v + " ] which does not match."
				return
			}
		}
	} else {
		v := version
		if len(v) == 0 {
			v = "*"
		}
		cmd := helmCmd([]string{"search", "repo", chart, "--version", v, "-l"}, "Validating [ "+chart+" ] chart's version [ "+version+" ] availability")

		if result := cmd.Exec(); result.code != 0 || strings.Contains(result.output, "No results found") {
			c <- "Chart [ " + chart + " ] with version [ " + version + " ] is specified for " +
				"apps [" + apps + "] but was not found. If this is not a local chart, define its helm repo in the helmRepo stanza in your DSF."
			return
		}
	}
}

// getChartInfo fetches the latest chart information (name, version) matching the semantic versioning constraints.
func getChartInfo(chart, version string) (*chartInfo, error) {
	if isLocalChart(chart) {
		log.Info("Chart [ " + chart + " ] with version [ " + version + " ] was found locally.")
	}

	cmd := helmCmd([]string{"show", "chart", chart, "--version", version}, "Getting latest non-local chart's version "+chart+"-"+version+"")

	result := cmd.Exec()
	if result.code != 0 {
		return nil, fmt.Errorf("Chart [ %s ] with version [ %s ] is specified but not found in the helm repositories", chart, version)
	}

	c := &chartInfo{}
	if err := yaml.Unmarshal([]byte(result.output), &c); err != nil {
		log.Fatal(fmt.Sprint(err))
	}

	return c, nil
}

// getHelmClientVersion returns Helm client Version
func getHelmVersion() string {
	cmd := helmCmd([]string{"version", "--short", "-c"}, "Checking Helm version")

	result := cmd.Exec()
	if result.code != 0 {
		log.Fatal("While checking helm version: " + result.errors)
	}

	return result.output
}

func checkHelmVersion(constraint string) bool {
	helmVersion := strings.TrimSpace(getHelmVersion())
	extractedHelmVersion := helmVersion
	if !strings.HasPrefix(helmVersion, "v") {
		extractedHelmVersion = strings.TrimSpace(strings.Split(helmVersion, ":")[1])
	}
	v, _ := version.NewVersion(extractedHelmVersion)
	jsonConstraint, _ := version.NewConstraint(constraint)
	if jsonConstraint.Check(v) {
		return true
	}
	return false
}

// helmPluginExists returns true if the plugin is present in the environment and false otherwise.
// It takes as input the plugin's name to check if it is recognizable or not. e.g. diff
func helmPluginExists(plugin string) bool {
	cmd := helmCmd([]string{"plugin", "list"}, "Validating that [ "+plugin+" ] is installed")

	result := cmd.Exec()

	if result.code != 0 {
		return false
	}

	return strings.Contains(result.output, plugin)
}

// updateChartDep updates dependencies for a local chart
func updateChartDep(chartPath string) error {
	cmd := helmCmd([]string{"dependency", "update", chartPath}, "Updating dependency for local chart [ "+chartPath+" ]")

	result := cmd.Exec()
	if result.code != 0 {
		return errors.New(result.errors)
	}
	return nil
}

// addHelmRepos adds repositories to Helm if they don't exist already.
// Helm does not mind if a repo with the same name exists. It treats it as an update.
func addHelmRepos(repos map[string]string) error {
	var helmRepos []helmRepo
	existingRepos := make(map[string]string)

	// get existing helm repositories
	cmdList := helmCmd(concat([]string{"repo", "list", "--output", "json"}), "Listing helm repositories")
	if reposResult := cmdList.Exec(); reposResult.code == 0 {
		if err := json.Unmarshal([]byte(reposResult.output), &helmRepos); err != nil {
			log.Fatal(fmt.Sprintf("failed to unmarshal Helm CLI output: %s", err))
		}
		// create map of existing repositories
		for _, repo := range helmRepos {
			existingRepos[repo.Name] = repo.Url
		}
	} else {
		if !strings.Contains(reposResult.errors, "no repositories to show") {
			return fmt.Errorf("while listing helm repositories: %s", reposResult.errors)
		}
	}

	repoAddFlags := ""
	if checkHelmVersion(">=3.3.2") {
		repoAddFlags += "--force-update"
	}

	for repoName, repoLink := range repos {
		basicAuthArgs := []string{}
		// check if repo is in GCS, then perform GCS auth -- needed for private GCS helm repos
		// failed auth would not throw an error here, as it is possible that the repo is public and does not need authentication
		if strings.HasPrefix(repoLink, "gs://") {
			if !helmPluginExists("gcs") {
				log.Fatal(fmt.Sprintf("repository %s can't be used: helm-gcs plugin is missing", repoLink))
			}
			msg, err := gcs.Auth()
			if err != nil {
				log.Fatal(msg)
			}
		}

		u, err := url.Parse(repoLink)
		if err != nil {
			log.Fatal("failed to add helm repo:  " + err.Error())
		}
		if u.User != nil {
			p, ok := u.User.Password()
			if !ok {
				log.Fatal("helm repo " + repoName + " has incomplete basic auth info. Missing the password!")
			}
			basicAuthArgs = append(basicAuthArgs, "--username", u.User.Username(), "--password", p)
			u.User = nil
			repoLink = u.String()
		}

		cmd := helmCmd(concat([]string{"repo", "add", repoAddFlags, repoName, repoLink}, basicAuthArgs), "Adding helm repository [ "+repoName+" ]")
		// check current repository against existing repositories map in order to make sure it's missing and needs to be added
		if existingRepoUrl, ok := existingRepos[repoName]; ok {
			if repoLink == existingRepoUrl {
				continue
			}
		}
		if result := cmd.Exec(); result.code != 0 {
			return fmt.Errorf("While adding helm repository ["+repoName+"]: %s", result.errors)
		}
	}

	if len(repos) > 0 {
		cmd := helmCmd([]string{"repo", "update"}, "Updating helm repositories")

		if result := cmd.Exec(); result.code != 0 {
			return errors.New("While updating helm repos : " + result.errors)
		}
	}

	return nil
}
