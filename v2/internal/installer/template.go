package installer

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/olekukonko/tablewriter"
	"github.com/projectdiscovery/gologger"
	"github.com/projectdiscovery/nuclei/v2/pkg/catalog/config"
	errorutil "github.com/projectdiscovery/utils/errors"
	fileutil "github.com/projectdiscovery/utils/file"
	stringsutil "github.com/projectdiscovery/utils/strings"
	updateutils "github.com/projectdiscovery/utils/update"
)

const (
	checkSumFilePerm = 0644
)

var (
	HideProgressBar        = true
	HideReleaseNotes       = false
	HideUpdateChangesTable = false
)

// TemplateUpdateResults contains the results of template update
type templateUpdateResults struct {
	additions     []string
	deletions     []string
	modifications []string
	totalCount    int
}

// String returns markdown table of template update results
func (t *templateUpdateResults) String() string {
	var buff bytes.Buffer
	data := [][]string{
		{strconv.Itoa(t.totalCount), strconv.Itoa(len(t.additions)), strconv.Itoa(len(t.deletions))},
	}
	table := tablewriter.NewWriter(&buff)
	table.SetHeader([]string{"Total", "Added", "Removed"})
	for _, v := range data {
		table.Append(v)
	}
	table.Render()
	return buff.String()
}

// TemplateManager is a manager for templates.
// It downloads / updates / installs templates.
type TemplateManager struct{}

// FreshInstallIfNotExists installs templates if they are not already installed
// if templates directory already exists, it does nothing
func (t *TemplateManager) FreshInstallIfNotExists() error {
	if fileutil.FolderExists(config.DefaultConfig.TemplatesDirectory) {
		return nil
	}
	gologger.Info().Msgf("nuclei-templates are not installed, installing...")
	return t.installTemplatesAt(config.DefaultConfig.TemplatesDirectory)
}

// UpdateIfOutdated updates templates if they are outdated
func (t *TemplateManager) UpdateIfOutdated() error {
	// if folder does not exist, its a fresh install and not update
	if !fileutil.FolderExists(config.DefaultConfig.TemplatesDirectory) {
		return t.FreshInstallIfNotExists()
	}
	if config.DefaultConfig.NeedsTemplateUpdate() {
		return t.updateTemplatesAt(config.DefaultConfig.TemplatesDirectory)
	}
	return nil
}

// installTemplatesAt installs templates at given directory
func (t *TemplateManager) installTemplatesAt(dir string) error {
	if !fileutil.FolderExists(dir) {
		if err := fileutil.CreateFolder(dir); err != nil {
			return errorutil.NewWithErr(err).Msgf("failed to create directory at %s", dir)
		}
	}
	ghrd, err := updateutils.NewghReleaseDownloader(config.OfficialNucleiTeamplatesRepoName)
	if err != nil {
		return errorutil.NewWithErr(err).Msgf("failed to install templates at %s", dir)
	}
	// write templates to disk
	if err := t.writeTemplatestoDisk(ghrd, dir); err != nil {
		return err
	}
	gologger.Info().Msgf("Successfully installed nuclei-templates at %s", dir)
	return nil
}

// updateTemplatesAt updates templates at given directory
func (t *TemplateManager) updateTemplatesAt(dir string) error {
	// firstly read checksums from .checksum file these are used to generate stats
	oldchecksums, err := t.getChecksumFromDir(dir)
	if err != nil {
		// if something went wrong overwrite all files
		oldchecksums = make(map[string]string)
	}

	ghrd, err := updateutils.NewghReleaseDownloader(config.OfficialNucleiTeamplatesRepoName)
	if err != nil {
		return errorutil.NewWithErr(err).Msgf("failed to install templates at %s", dir)
	}

	gologger.Info().Msgf("Your current nuclei-templates %s are outdated. Latest is %s\n", config.DefaultConfig.TemplateVersion, ghrd.Latest.GetTagName())

	// write templates to disk
	if err := t.writeTemplatestoDisk(ghrd, dir); err != nil {
		return err
	}

	// get checksums from new templates
	newchecksums, err := t.getChecksumFromDir(dir)
	if err != nil {
		// unlikely this case will happen
		return errorutil.NewWithErr(err).Msgf("failed to get checksums from %s after update", dir)
	}

	// summarize all changes
	results := t.summarizeChanges(oldchecksums, newchecksums)

	// print summary
	if results.totalCount > 0 {
		gologger.Info().Msgf("Successfully updated nuclei-templates (%v) to %s. GoodLuck!", ghrd.Latest.GetTagName(), dir)
		if !HideUpdateChangesTable {
			// print summary table
			gologger.Print().Msgf("\nNuclei Templates %s Changelog\n", ghrd.Latest.GetTagName())
			gologger.DefaultLogger.Print().Msg(results.String())
		}
	} else {
		gologger.Info().Msgf("Successfully updated nuclei-templates (%v) to %s. GoodLuck!", ghrd.Latest.GetTagName(), dir)
	}
	return nil
}

// summarizeChanges summarizes changes between old and new checksums
func (t *TemplateManager) summarizeChanges(old, new map[string]string) *templateUpdateResults {
	results := &templateUpdateResults{}
	for k, v := range new {
		if oldv, ok := old[k]; ok {
			if oldv != v {
				results.modifications = append(results.modifications, k)
			}
		} else {
			results.additions = append(results.additions, k)
		}
	}
	for k := range old {
		if _, ok := new[k]; !ok {
			results.deletions = append(results.deletions, k)
		}
	}
	results.totalCount = len(results.additions) + len(results.deletions) + len(results.modifications)
	return results
}

// getAbsoluteFilePath returns absolute path where a file should be written based on given uri(i.e files in zip)
// if returned path is empty, it means that file should not be written and skipped
func (t *TemplateManager) getAbsoluteFilePath(templatedir, uri string, f fs.FileInfo) string {
	// overwrite .nuclei-ignore everytime nuclei-templates are downloaded
	if f.Name() == config.NucleiIgnoreFileName {
		return config.DefaultConfig.GetIgnoreFilePath()
	}
	// skip all meta files
	if !strings.EqualFold(f.Name(), config.NewTemplateAdditionsFileName) {
		if strings.TrimSpace(f.Name()) == "" || strings.HasPrefix(f.Name(), ".") || strings.EqualFold(f.Name(), "README.md") {
			return ""
		}
	}

	// get root or leftmost directory name from path
	// this is in format `projectdiscovery-nuclei-templates-commithash`

	index := strings.Index(uri, "/")
	if index == -1 {
		// zip files does not have directory at all , in this case log error but continue
		gologger.Warning().Msgf("failed to get directory name from uri: %s", uri)
		return filepath.Join(templatedir, uri)
	}
	// seperator is also included in rootDir
	rootDirectory := uri[:index+1]
	relPath := strings.TrimPrefix(uri, rootDirectory)

	// if it is a github meta directory skip it
	if stringsutil.HasPrefixAny(relPath, ".github", ".git") {
		return ""
	}

	newPath := filepath.Clean(filepath.Join(templatedir, relPath))

	if !strings.HasPrefix(newPath, templatedir) {
		// we don't allow LFI
		return ""
	}

	if relPath != "" && f.IsDir() {
		// if uri is a directory, create it
		if err := fileutil.CreateFolder(newPath); err != nil {
			gologger.Warning().Msgf("uri %v: got %s while installing templates", uri, err)
		}
		return ""
	}
	return newPath
}

// writeChecksumFileInDir is actual method responsible for writing all templates to directory
func (t *TemplateManager) writeTemplatestoDisk(ghrd *updateutils.GHReleaseDownloader, dir string) error {
	callbackFunc := func(uri string, f fs.FileInfo, r io.Reader) error {
		writePath := t.getAbsoluteFilePath(dir, uri, f)
		if writePath == "" {
			// skip writing file
			return nil
		}
		bin, err := io.ReadAll(r)
		if err != nil {
			// if error occurs, iteration also stops
			return errorutil.NewWithErr(err).Msgf("failed to read file %s", uri)
		}
		return os.WriteFile(writePath, bin, f.Mode())
	}
	err := ghrd.DownloadSourceWithCallback(!HideProgressBar, callbackFunc)
	if err != nil {
		return err
	}
	if err := config.DefaultConfig.WriteTemplatesConfig(); err != nil {
		return err
	}
	// update ignore hash after writing new templates
	if err := config.DefaultConfig.UpdateNucleiIgnoreHash(); err != nil {
		return err
	}

	// update templates version in config file
	if err := config.DefaultConfig.SetTemplatesVersion(ghrd.Latest.GetTagName()); err != nil {
		return err
	}

	// after installation create and write checksums to .checksum file
	return t.writeChecksumFileInDir(dir)
}

// getChecksumFromDir returns a map containing checksums (md5 hash) of all yaml files (with .yaml extension)
// if .checksum file does not exist checksums are calculated and returned
func (t *TemplateManager) getChecksumFromDir(dir string) (map[string]string, error) {
	checksumFilePath := config.DefaultConfig.GetChecksumFilePath()
	if fileutil.FileExists(checksumFilePath) {
		checksums, err := os.ReadFile(checksumFilePath)
		if err == nil {
			allChecksums := make(map[string]string)
			for _, v := range strings.Split(string(checksums), "\n") {
				v = strings.TrimSpace(v)
				tmparr := strings.Split(v, ",")
				if len(tmparr) != 2 {
					continue
				}
				allChecksums[tmparr[0]] = tmparr[1]
			}
			return allChecksums, nil
		}
	}
	return t.calculateChecksumMap(dir)
}

// writeChecksumFileInDir creates checksums of all yaml files in given directory
// and writes them to a file named .checksum
func (t *TemplateManager) writeChecksumFileInDir(dir string) error {
	checksumMap, err := t.calculateChecksumMap(dir)
	if err != nil {
		return err
	}
	var buff bytes.Buffer
	for k, v := range checksumMap {
		buff.WriteString(k + "," + v)
	}
	return os.WriteFile(config.DefaultConfig.GetChecksumFilePath(), buff.Bytes(), checkSumFilePerm)
}

// getChecksumMap returns a map containing checksums (md5 hash) of all yaml files (with .yaml extension)
func (t *TemplateManager) calculateChecksumMap(dir string) (map[string]string, error) {
	// getchecksumMap walks given directory `dir` and returns a map containing
	// checksums (md5 hash) of all yaml files (with .yaml extension) and the
	// format is map[filePath]checksum
	checksumMap := map[string]string{}

	getChecksum := func(filepath string) (string, error) {
		// return md5 hash of the file
		bin, err := os.ReadFile(filepath)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%x", md5.Sum(bin)), nil
	}

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// skip checksums of custom templates i.e github and s3
		if stringsutil.HasPrefixAny(path, config.DefaultConfig.CustomGithubTemplatesDirectory, config.DefaultConfig.CustomS3TemplatesDirectory) {
			return nil
		}

		// current implementations calculates checksums of all files (including .yaml,.txt,.md,.json etc)
		if !d.IsDir() {
			checksum, err := getChecksum(path)
			if err != nil {
				return err
			}
			checksumMap[path] = checksum
		}
		return nil
	})
	return checksumMap, err
}
