package osvscanner

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/osv-scanner/internal/sbom"
	"github.com/google/osv-scanner/pkg/config"
	"github.com/google/osv-scanner/pkg/lockfile"
	"github.com/google/osv-scanner/pkg/models"
	"github.com/google/osv-scanner/pkg/osv"
	"github.com/google/osv-scanner/pkg/output"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

type ScannerActions struct {
	LockfilePaths        []string
	SBOMPaths            []string
	DirectoryPaths       []string
	GitCommits           []string
	Recursive            bool
	SkipGit              bool
	NoIgnore             bool
	DockerContainerNames []string
	ConfigOverridePath   string
}

// NoPackagesFoundErr for when no packages is found during a scan.
//
//nolint:errname,stylecheck // Would require version bump to change
var NoPackagesFoundErr = errors.New("no packages found in scan")

//nolint:errname,stylecheck // Would require version bump to change
var VulnerabilitiesFoundErr = errors.New("vulnerabilities found")

// scanDir walks through the given directory to try to find any relevant files
// These include:
//   - Any lockfiles with scanLockfile
//   - Any SBOM files with scanSBOMFile
//   - Any git repositories with scanGit
func scanDir(r *output.Reporter, query *osv.BatchedQuery, dir string, skipGit bool, recursive bool, useGitIgnore bool) error {
	var ignoreMatcher *gitIgnoreMatcher
	if useGitIgnore {
		var err error
		ignoreMatcher, err = parseGitIgnores(dir)
		if err != nil {
			r.PrintError(fmt.Sprintf("Unable to parse git ignores: %v", err))
			useGitIgnore = false
		}
	}

	root := true

	return filepath.WalkDir(dir, func(path string, info os.DirEntry, err error) error {
		if err != nil {
			r.PrintText(fmt.Sprintf("Failed to walk %s: %v\n", path, err))
			return err
		}

		path, err = filepath.Abs(path)
		if err != nil {
			r.PrintError(fmt.Sprintf("Failed to walk path %s\n", err))
			return err
		}

		if useGitIgnore {
			match, err := ignoreMatcher.match(path, info.IsDir())
			if err != nil {
				r.PrintText(fmt.Sprintf("Failed to resolve gitignore for %s: %v", path, err))
				// Don't skip if we can't parse now - potentially noisy for directories with lots of items
			} else if match {
				if info.IsDir() {
					return filepath.SkipDir
				}

				return nil
			}
		}

		if !skipGit && info.IsDir() && info.Name() == ".git" {
			err := scanGit(r, query, filepath.Dir(path)+"/")
			if err != nil {
				r.PrintText(fmt.Sprintf("scan failed for git repository, %s: %v\n", path, err))
				// Not fatal, so don't return and continue scanning other files
			}

			return filepath.SkipDir
		}

		if !info.IsDir() {
			if parser, _ := lockfile.FindParser(path, ""); parser != nil {
				err := scanLockfile(r, query, path, "")
				if err != nil {
					r.PrintError(fmt.Sprintf("Attempted to scan lockfile but failed: %s\n", path))
				}
			}
			// No need to check for error
			// If scan fails, it means it isn't a valid SBOM file,
			// so just move onto the next file
			_ = scanSBOMFile(r, query, path)
		}

		if !root && !recursive && info.IsDir() {
			return filepath.SkipDir
		}
		root = false

		return nil
	})
}

type gitIgnoreMatcher struct {
	matcher  gitignore.Matcher
	repoPath string
}

func parseGitIgnores(dir string) (*gitIgnoreMatcher, error) {
	// We need to parse .gitignore files from the root of the git repo to correctly identify ignored files
	// Defaults to current directory if dir is not in a repo or some other error
	// TODO: Won't parse ignores if dir is not in a git repo, and is not under the current directory (e.g ../path/to)
	fs := osfs.New(".")
	if repo, err := git.PlainOpenWithOptions(dir, &git.PlainOpenOptions{DetectDotGit: true}); err == nil {
		if tree, err := repo.Worktree(); err == nil {
			fs = tree.Filesystem
		}
	}

	patterns, err := gitignore.ReadPatterns(fs, []string{"."})
	if err != nil {
		return nil, err
	}
	matcher := gitignore.NewMatcher(patterns)
	path, err := filepath.Abs(fs.Root())
	if err != nil {
		return nil, err
	}

	return &gitIgnoreMatcher{matcher: matcher, repoPath: path}, nil
}

// gitIgnoreMatcher.match will return true if the file/directory matches a gitignore entry
// i.e. true if it should be ignored
func (m *gitIgnoreMatcher) match(absPath string, isDir bool) (bool, error) {
	pathInGit, err := filepath.Rel(m.repoPath, absPath)
	if err != nil {
		return false, err
	}
	// must prepend "." to paths because of how gitignore.ReadPatterns interprets paths
	pathInGitSep := append([]string{"."}, strings.Split(pathInGit, string(filepath.Separator))...)

	return m.matcher.Match(pathInGitSep, isDir), nil
}

// scanLockfile will load, identify, and parse the lockfile path passed in, and add the dependencies specified
// within to `query`
func scanLockfile(r *output.Reporter, query *osv.BatchedQuery, path string, parseAs string) error {
	var err error
	var parsedLockfile lockfile.Lockfile

	// special case for the APK parser because it has a very generic name while
	// living at a specific location, so it's not included in the map of parsers
	// used by lockfile.Parse to avoid false-positives when scanning projects
	if parseAs == "apk-installed" {
		parsedLockfile, err = lockfile.FromApkInstalled(path)
	} else {
		parsedLockfile, err = lockfile.Parse(path, parseAs)
	}

	if err != nil {
		return err
	}
	parsedAsComment := ""

	if parseAs != "" {
		parsedAsComment = fmt.Sprintf("as a %s ", parseAs)
	}

	r.PrintText(fmt.Sprintf("Scanned %s file %sand found %d packages\n", path, parsedAsComment, len(parsedLockfile.Packages)))

	for _, pkgDetail := range parsedLockfile.Packages {
		pkgDetailQuery := osv.MakePkgRequest(pkgDetail)
		pkgDetailQuery.Source = models.SourceInfo{
			Path: path,
			Type: "lockfile",
		}
		query.Queries = append(query.Queries, pkgDetailQuery)
	}

	return nil
}

// scanSBOMFile will load, identify, and parse the SBOM path passed in, and add the dependencies specified
// within to `query`
func scanSBOMFile(r *output.Reporter, query *osv.BatchedQuery, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	for _, provider := range sbom.Providers {
		if provider.Name() == "SPDX" &&
			!strings.Contains(strings.ToLower(filepath.Base(path)), ".spdx") {
			// All spdx files should have the .spdx in the filename, even if
			// it's not the extension:  https://spdx.github.io/spdx-spec/v2.3/conformance/
			// Skip if this isn't the case to avoid panics
			continue
		}
		count := 0
		err := provider.GetPackages(file, func(id sbom.Identifier) error {
			purlQuery := osv.MakePURLRequest(id.PURL)
			purlQuery.Source = models.SourceInfo{
				Path: path,
				Type: "sbom",
			}
			query.Queries = append(query.Queries, purlQuery)
			count++

			return nil
		})
		if err == nil {
			// Found the right format.
			r.PrintText(fmt.Sprintf("Scanned %s SBOM and found %d packages\n", provider.Name(), count))
			return nil
		}

		if errors.Is(err, sbom.ErrInvalidFormat) {
			continue
		}

		return err
	}

	return nil
}

func getCommitSHA(repoDir string) (string, error) {
	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		return "", err
	}
	head, err := repo.Head()
	if err != nil {
		return "", err
	}

	return head.Hash().String(), nil
}

// Scan git repository. Expects repoDir to end with /
func scanGit(r *output.Reporter, query *osv.BatchedQuery, repoDir string) error {
	commit, err := getCommitSHA(repoDir)
	if err != nil {
		return err
	}
	r.PrintText(fmt.Sprintf("Scanning %s at commit %s\n", repoDir, commit))

	return scanGitCommit(query, commit, repoDir)
}

func scanGitCommit(query *osv.BatchedQuery, commit string, source string) error {
	gitQuery := osv.MakeCommitRequest(commit)
	gitQuery.Source = models.SourceInfo{
		Path: source,
		Type: "git",
	}
	query.Queries = append(query.Queries, gitQuery)

	return nil
}

func scanDebianDocker(r *output.Reporter, query *osv.BatchedQuery, dockerImageName string) error {
	cmd := exec.Command("docker", "run", "--rm", "--entrypoint", "/usr/bin/dpkg-query", dockerImageName, "-f", "${Package}###${Version}\\n", "-W")
	stdout, err := cmd.StdoutPipe()

	if err != nil {
		r.PrintError(fmt.Sprintf("Failed to get stdout: %s\n", err))
		return err
	}
	err = cmd.Start()
	if err != nil {
		r.PrintError(fmt.Sprintf("Failed to start docker image: %s\n", err))
		return err
	}
	// TODO: Do error checking here
	//nolint:errcheck
	defer cmd.Wait()
	scanner := bufio.NewScanner(stdout)
	packages := 0
	for scanner.Scan() {
		text := scanner.Text()
		text = strings.TrimSpace(text)
		if len(text) == 0 {
			continue
		}
		splitText := strings.Split(text, "###")
		if len(splitText) != 2 {
			r.PrintError(fmt.Sprintf("Unexpected output from Debian container: \n\n%s\n", text))
			return fmt.Errorf("unexpected output from Debian container: \n\n%s", text)
		}
		pkgDetailsQuery := osv.MakePkgRequest(lockfile.PackageDetails{
			Name:    splitText[0],
			Version: splitText[1],
			// TODO(rexpan): Get and specify exact debian release version
			Ecosystem: "Debian",
		})
		pkgDetailsQuery.Source = models.SourceInfo{
			Path: dockerImageName,
			Type: "docker",
		}
		query.Queries = append(query.Queries, pkgDetailsQuery)
		packages += 1
	}
	r.PrintText(fmt.Sprintf("Scanned docker image with %d packages\n", packages))

	return nil
}

// Filters response according to config, returns number of responses removed
func filterResponse(r *output.Reporter, query osv.BatchedQuery, resp *osv.BatchedResponse, configManager *config.ConfigManager) int {
	hiddenVulns := map[string]config.IgnoreEntry{}

	for i, result := range resp.Results {
		var filteredVulns []osv.MinimalVulnerability
		configToUse := configManager.Get(r, query.Queries[i].Source.Path)
		for _, vuln := range result.Vulns {
			ignore, ignoreLine := configToUse.ShouldIgnore(vuln.ID)
			if ignore {
				hiddenVulns[vuln.ID] = ignoreLine
			} else {
				filteredVulns = append(filteredVulns, vuln)
			}
		}
		resp.Results[i].Vulns = filteredVulns
	}

	for id, ignoreLine := range hiddenVulns {
		r.PrintText(fmt.Sprintf("%s has been filtered out because: %s\n", id, ignoreLine.Reason))
	}

	return len(hiddenVulns)
}

func parseLockfilePath(lockfileElem string) (string, string) {
	if !strings.Contains(lockfileElem, ":") {
		lockfileElem = ":" + lockfileElem
	}

	splits := strings.SplitN(lockfileElem, ":", 2)

	return splits[0], splits[1]
}

// Perform osv scanner action, with optional reporter to output information
func DoScan(actions ScannerActions, r *output.Reporter) (models.VulnerabilityResults, error) {
	if r == nil {
		r = output.NewVoidReporter()
	}

	configManager := config.ConfigManager{
		DefaultConfig: config.Config{},
		ConfigMap:     make(map[string]config.Config),
	}

	var query osv.BatchedQuery

	if actions.ConfigOverridePath != "" {
		err := configManager.UseOverride(actions.ConfigOverridePath)
		if err != nil {
			r.PrintError(fmt.Sprintf("Failed to read config file: %s\n", err))
			return models.VulnerabilityResults{}, err
		}
	}

	for _, container := range actions.DockerContainerNames {
		// TODO: Automatically figure out what docker base image
		// and scan appropriately.
		_ = scanDebianDocker(r, &query, container)
	}

	for _, lockfileElem := range actions.LockfilePaths {
		parseAs, lockfilePath := parseLockfilePath(lockfileElem)
		lockfilePath, err := filepath.Abs(lockfilePath)
		if err != nil {
			r.PrintError(fmt.Sprintf("Failed to resolved path with error %s\n", err))
			return models.VulnerabilityResults{}, err
		}
		err = scanLockfile(r, &query, lockfilePath, parseAs)
		if err != nil {
			return models.VulnerabilityResults{}, err
		}
	}

	for _, sbomElem := range actions.SBOMPaths {
		sbomElem, err := filepath.Abs(sbomElem)
		if err != nil {
			return models.VulnerabilityResults{}, fmt.Errorf("failed to resolved path with error %w", err)
		}
		err = scanSBOMFile(r, &query, sbomElem)
		if err != nil {
			return models.VulnerabilityResults{}, err
		}
	}

	for _, commit := range actions.GitCommits {
		err := scanGitCommit(&query, commit, "HASH")
		if err != nil {
			return models.VulnerabilityResults{}, err
		}
	}

	for _, dir := range actions.DirectoryPaths {
		r.PrintText(fmt.Sprintf("Scanning dir %s\n", dir))
		err := scanDir(r, &query, dir, actions.SkipGit, actions.Recursive, !actions.NoIgnore)
		if err != nil {
			return models.VulnerabilityResults{}, err
		}
	}

	if len(query.Queries) == 0 {
		return models.VulnerabilityResults{}, NoPackagesFoundErr
	}

	resp, err := osv.MakeRequest(query)
	if err != nil {
		return models.VulnerabilityResults{}, fmt.Errorf("scan failed %w", err)
	}

	filtered := filterResponse(r, query, resp, &configManager)
	if filtered > 0 {
		r.PrintText(fmt.Sprintf("Filtered %d vulnerabilities from output\n", filtered))
	}

	hydratedResp, err := osv.Hydrate(resp)
	if err != nil {
		return models.VulnerabilityResults{}, fmt.Errorf("failed to hydrate OSV response: %w", err)
	}

	vulnerabilityResults := groupResponseBySource(r, query, hydratedResp)
	// if vulnerability exists it should return error
	if len(vulnerabilityResults.Results) > 0 {
		return vulnerabilityResults, VulnerabilitiesFoundErr
	}

	return vulnerabilityResults, nil
}
