//   .-.   .-.
//   | |   | |
//   | `--.| `--.
//   `----'`----'
//
// ll – a small utility to list files in the current directory.
//
// # Why?
// Because I wanted to display files in columns with git status.
//
// # Rationalize
// One entry per line for lots of files can't be fitted on a screen
// and requires scrolling. With the multi-column layout, space can be
// used more efficiently. At the same time, git status information is
// also often needed.

package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	. "strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	modified      = "\033[0;34m%s\033[0m"
	added         = "\033[0;32m%s\033[0m"
	untracked     = "\033[0;31m%s\033[0m"
	ignored       = "\033[0;90m%s\033[0m"
	bold          = "\033[1m%v\033[0m"
	folderIcon    = "📁 "
	filePrefix    = "   "
	symlinkSuffix = "~>"
	branchIcon    = "⎇"
	aheadIcon     = "↑"
	behindIcon    = "↓"
	stagedAdd     = "+"
	stagedMod     = "~"
	stagedDel     = "-"
)

var (
	spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	sizes   = []string{"B", "kB", "MB", "GB", "TB", "PB", "EB"}
	base    = float64(1000)
)

var showAll bool

func main() {
	args := []string{}
	for _, arg := range os.Args[1:] {
		if arg == "-a" || arg == "--all" {
			showAll = true
		} else {
			args = append(args, arg)
		}
	}

	if len(args) == 1 {
		ll(args[0])
		return
	}

	if len(args) > 1 {
		for _, arg := range args {
			path, _ := filepath.Abs(arg)
			printInfo(fileInfo(path), path)
		}
		return
	}

	pwd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	ll(pwd)
}

func ll(cwd string) {
	// Maybe it is and argument, so get absolute path.
	cwd, _ = filepath.Abs(cwd)

	// Is it a file?
	if fi := fileInfo(cwd); !fi.IsDir() {
		printInfo(fi, cwd)
		return
	}

	// ReadDir already returns files and dirs sorted by filename.
	files, err := ioutil.ReadDir(cwd)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(files) == 0 {
		return
	}

	// Sort: directories first (including symlinks to dirs), then case-insensitive alphabetical
	sort.Slice(files, func(i, j int) bool {
		iSym := files[i].Mode()&os.ModeSymlink != 0
		jSym := files[j].Mode()&os.ModeSymlink != 0
		iDir := files[i].IsDir() || (iSym && isSymlinkToDir(filepath.Join(cwd, files[i].Name())))
		jDir := files[j].IsDir() || (jSym && isSymlinkToDir(filepath.Join(cwd, files[j].Name())))
		if iDir != jDir {
			return iDir // directories first
		}
		return ToLower(files[i].Name()) < ToLower(files[j].Name())
	})

	// Get gitignored files set
	gitIgnored := getGitIgnored(cwd, files)

	// Filter out gitignored files unless -a flag is set
	if !showAll {
		files = filterByIgnored(files, gitIgnored)
		if len(files) == 0 {
			return
		}
	}

	// Print git status bar if in a git repo and stdout is a terminal
	stdoutInfo, err := os.Stdout.Stat()
	if err != nil {
		panic(err)
	}
	isTTY := (stdoutInfo.Mode() & os.ModeCharDevice) != 0
	if isTTY {
		printGitStatusBar(cwd)
	}

	// We need terminal size to nicely fit on screen.
	var width, height int
	ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws == nil {
		width, height = 80, 60
	} else {
		width, height = int(ws.Col), int(ws.Row)
	}

	// If it's possible to fit all files in one column on half of screen, just use one column.
	// Otherwise let's squeeze listing in half of screen.
	columns := len(files)/(height/2) + 1

	// Gonna keep file names and format string for git status.
	modes := map[string]string{}

	// If stdout of ll piped, use ls behavior: one line, no colors.
	if !isTTY {
		columns = 1
	} else {
		// BUG FIX: Pass cwd to gitStatus so git commands run in the target directory
		status := gitStatus(cwd)
		for _, file := range files {
			name := file.Name()
			fullFilePath := filepath.Join(cwd, file.Name())
			isSymlink := file.Mode()&os.ModeSymlink != 0
			if file.IsDir() || (isSymlink && isSymlinkToDir(fullFilePath)) {
				name = name + "/" // No icon in key - just name with /
			}
			if isSymlink {
				name += symlinkSuffix
			}

			// Mark gitignored files in gray (when -a is used)
			if gitIgnored[file.Name()] {
				modes[name] = ignored
				continue
			}

			// gitStatus returns file names of modified files from repo root.
			fullPath := filepath.Join(cwd, name)
			for path, mode := range status {
				// BUG FIX: Check if the git status path starts with fullPath (for directories)
				// or if they match exactly (for files)
				if hasPathPrefix(path, fullPath) {
					if mode[0] == '?' || mode[1] == '?' {
						modes[name] = untracked
					} else if mode[0] == 'A' || mode[1] == 'A' {
						modes[name] = added
					} else if mode[0] == 'M' || mode[1] == 'M' {
						modes[name] = modified
					}
				}
			}
		}
	}

start:
	// Let's try to fit everything in terminal width with this many columns.
	// If we are not able to do it, decrease column number and goto start.
	rows := int(math.Ceil(float64(len(files)) / float64(columns)))
	names := make([][]string, columns)    // names without folder icon (for width calc)
	prefixes := make([][]string, columns) // folder icon prefix (added at display)
	n := 0
	for i := 0; i < columns; i++ {
		names[i] = make([]string, rows)
		prefixes[i] = make([]string, rows)
		// Columns size is going to be of max file name size.
		max := 0
		for j := 0; j < rows; j++ {
			name := ""
			prefix := ""
			if n < len(files) {
				name = files[n].Name()
				fullFilePath := filepath.Join(cwd, name)
				isSymlink := files[n].Mode()&os.ModeSymlink != 0
				isDir := files[n].IsDir() || (isSymlink && isSymlinkToDir(fullFilePath))

				if isDir {
					// Dir should have icon and slash at end.
					prefix = folderIcon
					name = name + "/"
				} else {
					// Files get spacing prefix to align with folder icon
					prefix = filePrefix
				}
				if isSymlink {
					name += symlinkSuffix
				}
				n++
			}
			if max < len(name) {
				max = len(name)
			}
			names[i][j] = name
			prefixes[i][j] = prefix
		}
		// Append spaces to make all names in one column of same size.
		for j := 0; j < rows; j++ {
			names[i][j] += Repeat(" ", max-len(names[i][j]))
		}
	}

	const separator = "    " // Separator between columns.
	for j := 0; j < rows; j++ {
		row := make([]string, columns)
		for i := 0; i < columns; i++ {
			row[i] = names[i][j]
		}
		if len(Join(row, separator)) > width && columns > 1 {
			// Yep. No luck, let's decrease number of columns and try one more time.
			columns--
			goto start
		}
	}

	// Let's add colors from git status to file names.
	output := make([]string, rows)
	for j := 0; j < rows; j++ {
		row := make([]string, columns)
		for i := 0; i < columns; i++ {
			f, ok := modes[TrimRight(names[i][j], " ")]
			if !ok {
				f = "%s"
			}
			// Add folder icon prefix (doesn't affect column alignment)
			row[i] = prefixes[i][j] + fmt.Sprintf(f, names[i][j])
		}
		output[j] = Join(row, separator)
	}
	fmt.Println(Join(output, "\n"))
	fmt.Println()
}

// isSymlinkToDir checks if a symlink points to a directory
func isSymlinkToDir(path string) bool {
	target, err := os.Stat(path) // Stat follows symlinks
	if err != nil {
		return false // broken symlink or error
	}
	return target.IsDir()
}

// hasPathPrefix checks if filePath starts with dirPath.
// This is used to determine if a modified file is inside a directory.
// For example: hasPathPrefix("/repo/src/main.go", "/repo/src/") returns true
// Also handles exact matches for files.
func hasPathPrefix(filePath string, dirPath string) bool {
	// Normalize paths
	filePath = filepath.Clean(filePath)
	dirPath = filepath.Clean(dirPath)

	// Exact match (for files)
	if filePath == dirPath {
		return true
	}

	// Check if filePath is inside dirPath (for directories)
	// Add separator to avoid matching /repo/src with /repo/srcfile
	dirWithSep := dirPath
	if !HasSuffix(dirWithSep, string(filepath.Separator)) {
		dirWithSep += string(filepath.Separator)
	}

	return HasPrefix(filePath, dirWithSep)
}

// getGitIgnored returns a set of filenames that are ignored by git.
// Uses `git check-ignore` for accurate gitignore pattern matching.
func getGitIgnored(cwd string, files []os.FileInfo) map[string]bool {
	ignored := make(map[string]bool)

	// Check if we're in a git repo
	_, err := gitRepo(cwd)
	if err != nil {
		return ignored // Not a git repo, return empty set
	}

	// Build list of file paths to check
	var paths []string
	for _, file := range files {
		paths = append(paths, file.Name())
	}

	// Use git check-ignore to find ignored files
	cmd := exec.Command("git", append([]string{"check-ignore", "--"}, paths...)...)
	cmd.Dir = cwd
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Run() // Ignore error - exit code 1 means no files are ignored

	// Build set of ignored files
	for _, line := range Split(Trim(out.String(), "\n"), "\n") {
		if len(line) > 0 {
			ignored[line] = true
		}
	}

	return ignored
}

// filterByIgnored filters out files that are in the ignored set.
func filterByIgnored(files []os.FileInfo, ignored map[string]bool) []os.FileInfo {
	var filtered []os.FileInfo
	for _, file := range files {
		if !ignored[file.Name()] {
			filtered = append(filtered, file)
		}
	}
	return filtered
}

// BUG FIX: Added cwd parameter to run git command in the target directory
func gitRepo(cwd string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = cwd // Run git command in the target directory
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return Trim(out.String(), "\n"), err
}

// BUG FIX: Added cwd parameter to run git command in the target directory
func gitStatus(cwd string) map[string]string {
	repo, err := gitRepo(cwd)
	if err != nil {
		return nil
	}
	cmd := exec.Command("git", "status", "--porcelain=v1")
	cmd.Dir = cwd // Run git command in the target directory
	var out bytes.Buffer
	cmd.Stdout = &out
	err = cmd.Run()
	if err != nil {
		return nil
	}
	m := map[string]string{}
	for _, line := range Split(Trim(out.String(), "\n"), "\n") {
		if len(line) == 0 {
			continue
		}
		m[filepath.Join(repo, line[3:])] = line[:2]
	}
	return m
}

// gitBranch returns the current branch name
func gitBranch(cwd string) string {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = cwd
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return ""
	}
	return Trim(out.String(), "\n")
}

// gitAheadBehind returns ahead and behind counts relative to upstream
func gitAheadBehind(cwd string) (int, int) {
	cmd := exec.Command("git", "rev-list", "--left-right", "--count", "HEAD...@{upstream}")
	cmd.Dir = cwd
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return 0, 0
	}
	parts := Split(Trim(out.String(), "\n"), "\t")
	if len(parts) != 2 {
		return 0, 0
	}
	var ahead, behind int
	fmt.Sscanf(parts[0], "%d", &ahead)
	fmt.Sscanf(parts[1], "%d", &behind)
	return ahead, behind
}

// gitStagedCounts returns counts of staged added, modified, and deleted files
func gitStagedCounts(cwd string) (int, int, int) {
	cmd := exec.Command("git", "status", "--porcelain=v1")
	cmd.Dir = cwd
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return 0, 0, 0
	}
	var addCount, modCount, delCount int
	for _, line := range Split(Trim(out.String(), "\n"), "\n") {
		if len(line) < 2 {
			continue
		}
		index := line[0] // First char is index (staged) status
		switch index {
		case 'A':
			addCount++
		case 'M':
			modCount++
		case 'D':
			delCount++
		case 'R': // Renamed counts as mod
			modCount++
		}
	}
	return addCount, modCount, delCount
}

// printGitStatusBar prints the git status bar if in a git repo
func printGitStatusBar(cwd string) {
	branch := gitBranch(cwd)
	if branch == "" {
		return
	}

	var parts []string

	// Branch
	parts = append(parts, branchIcon+" "+branch)

	// Ahead/behind
	ahead, behind := gitAheadBehind(cwd)
	if ahead > 0 || behind > 0 {
		var ab []string
		if ahead > 0 {
			ab = append(ab, fmt.Sprintf("%s%d", aheadIcon, ahead))
		}
		if behind > 0 {
			ab = append(ab, fmt.Sprintf("%s%d", behindIcon, behind))
		}
		parts = append(parts, Join(ab, " "))
	}

	// Staged counts
	addCount, modCount, delCount := gitStagedCounts(cwd)
	if addCount > 0 || modCount > 0 || delCount > 0 {
		var staged []string
		if addCount > 0 {
			staged = append(staged, fmt.Sprintf("%s%d", stagedAdd, addCount))
		}
		if modCount > 0 {
			staged = append(staged, fmt.Sprintf("%s%d", stagedMod, modCount))
		}
		if delCount > 0 {
			staged = append(staged, fmt.Sprintf("%s%d", stagedDel, delCount))
		}
		parts = append(parts, Join(staged, " "))
	}

	fmt.Println(Join(parts, "  "))
}

func printInfo(fi os.FileInfo, path string) {
	name := fi.Name()
	size := fi.Size()
	isSymlink := fi.Mode()&os.ModeSymlink != 0
	isDir := fi.IsDir() || (isSymlink && isSymlinkToDir(path))

	if isDir {
		name = folderIcon + name + "/"
		if isSymlink {
			name += symlinkSuffix
		}
		done := make(chan bool)
		wg := sync.WaitGroup{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			i, t := 0, time.Tick(100*time.Millisecond)
			for {
				select {
				case <-t:
					fmt.Printf("\r%v\t%v", spinner[i%len(spinner)], name)
					i++
				case <-done:
					fmt.Print("\r")
					return
				}
			}
		}()
		size, _ = dirSize(path)
		done <- true
		wg.Wait()
	} else if isSymlink {
		name += symlinkSuffix
	}
	fmt.Printf("%v\t%v\n", toHuman(size), name)
}

func fileInfo(path string) os.FileInfo {
	fi, err := os.Lstat(path) // Use Lstat to detect symlinks
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return fi
}

func toHuman(s int64) string {
	if s < 10 {
		value := fmt.Sprintf(bold, s)
		return fmt.Sprintf("  %v B", value)
	}
	e := math.Floor(math.Log(float64(s)) / math.Log(base))
	suffix := sizes[int(e)]
	val := math.Floor(float64(s)/math.Pow(base, e)*10+0.5) / 10
	f := "%3.0f"
	if val < 10 {
		f = "%3.1f"
	}

	value := fmt.Sprintf(bold, fmt.Sprintf(f, val))
	return fmt.Sprintf("%v %v", value, suffix)
}

func dirSize(path string) (int64, error) {
	var size int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return err
	})
	return size, err
}
