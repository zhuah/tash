package main

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/cosiner/argv"
	"github.com/mattn/go-zglob"
	"github.com/uiez/tash/syntax"
)

type logger interface {
	debugln(v ...interface{})
	infoln(v ...interface{})
	warnln(v ...interface{})
	fatalln(v ...interface{})
}

func stringAtAndTrim(s []string, i int) string {
	if i < len(s) {
		return strings.TrimSpace(s[i])
	}
	return ""
}

func stringSplitAndTrimFilterSpace(s, sep string) []string {
	secs := stringSplitAndTrim(s, sep)
	var end int
	for i := range secs {
		if secs[i] != "" {
			if i != end {
				secs[end] = secs[i]
			}
			end++
		}
	}
	return secs[:end]
}

func stringSplitAndTrim(s, sep string) []string {
	secs := strings.Split(s, sep)
	for i := range secs {
		secs[i] = strings.TrimSpace(secs[i])
	}
	return secs
}

func stringSplitAndTrimToPair(s, sep string) (s1, s2 string) {
	var secs []string
	if sep == " " {
		secs = stringSplitAndTrimFilterSpace(s, sep)
	} else {
		secs = strings.SplitN(s, sep, 2)
	}
	return stringAtAndTrim(secs, 0), stringAtAndTrim(secs, 1)
}

func copyFile(dst, src string) error {
	srcFd, err := os.OpenFile(src, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	srcStat, err := srcFd.Stat()
	if err != nil {
		return err
	}
	defer srcFd.Close()
	dstFd, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer dstFd.Close()
	_, err = io.Copy(dstFd, srcFd)
	if err == nil {
		err = os.Chmod(dst, srcStat.Mode())
	}
	if err != nil {
		os.Remove(dst)
		return err
	}
	return nil
}

func copyPath(dst, src string) error {
	stat, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("read source path status failed: %w", err)
	}
	err = os.RemoveAll(dst)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove dst path failed: %w", err)
	}
	err = os.MkdirAll(filepath.Dir(dst), 0755)
	if err != nil {
		return fmt.Errorf("create dst path dirs failed: %w", err)
	}
	if !stat.IsDir() {
		err = os.MkdirAll(filepath.Dir(dst), 0755)
		if err != nil {
			return fmt.Errorf("create dst parent directory tree failed: %w", err)
		}
		return copyFile(dst, src)
	}
	dirChmods := map[string]os.FileMode{}
	err = filepath.Walk(src, func(srcPath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relPath, err := filepath.Rel(src, srcPath)
		if err != nil {
			return err
		}
		dstPath := filepath.Join(dst, relPath)
		if info.IsDir() {
			err = os.Mkdir(dstPath, 0755)
			if err != nil {
				return err
			}
			if info.Mode() != 0755 {
				dirChmods[dstPath] = info.Mode()
			}
			return nil
		}
		return copyFile(dstPath, srcPath)
	})
	if err != nil {
		return fmt.Errorf("copy path tree failed: %w", err)
	}
	for dir, mode := range dirChmods {
		err = os.Chmod(dir, mode)
		if err != nil {
			return fmt.Errorf("fix dir mod failed: %w", err)
		}
	}
	return nil
}

func checkHash(log logger, path string, alg, sig string, r io.Reader) bool {
	var hashCreator func() hash.Hash
	switch alg {
	case syntax.ResourceHashAlgSha1:
		hashCreator = sha1.New
	case syntax.ResourceHashAlgMD5:
		hashCreator = md5.New
	case syntax.ResourceHashAlgSha256:
		hashCreator = sha256.New
	}
	if hashCreator == nil || sig == "" {
		log.fatalln("invalid hash alg or sig:", path)
		return false
	}
	h := hashCreator()
	_, err := io.Copy(h, r)
	if err != nil {
		log.fatalln("check hash failed:", path, err)
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == strings.ToLower(sig)
}

func downloadFile(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("create download request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("failed to fetch resource: %s", resp.Status)
	}

	fd, err := ioutil.TempFile("", "tash*")
	if err != nil {
		return "", fmt.Errorf("create tmp file failed: %w", err)
	}
	defer fd.Close()
	_, err = io.Copy(fd, resp.Body)
	if err != nil {
		os.Remove(fd.Name())
		return "", fmt.Errorf("download file failed: %w", err)
	}
	return fd.Name(), nil
}

type commandFds struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

func execCommand(envs *ExpandEnvs, sections [][]string, cmdDir string, needsOutput bool, fds commandFds, background bool) (pid int, output string, err error) {
	if len(sections) == 0 {
		return 0, "", fmt.Errorf("empty command line string")
	}
	cmds, err := argv.Cmds(sections...)
	if err != nil {
		return 0, "", fmt.Errorf("build command failed: %s", err)
	}
	osEnvs := envs.formatEnvs()
	for i := range cmds {
		cmds[i].Env = osEnvs
		if cmdDir != "" {
			cmds[i].Dir = cmdDir
		}
	}
	if needsOutput {
		fds.Stdin = nil
		fds.Stdout = bytes.NewBuffer(nil)
		fds.Stderr = nil
	}
	if background {
		err = argv.Start(fds.Stdin, fds.Stdout, fds.Stderr, cmds...)
	} else {
		err = argv.Pipe(fds.Stdin, fds.Stdout, fds.Stderr, cmds...)
	}
	if err != nil {
		return 0, "", fmt.Errorf("run command failed: %s", err)
	}
	if p := cmds[len(cmds)-1].Process; p != nil {
		pid = p.Pid
	}
	if needsOutput {
		return pid, strings.TrimSpace(fds.Stdout.(*bytes.Buffer).String()), nil
	}
	return pid, "", nil
}

func runCommand(envs *ExpandEnvs, cmd, cmdDir string, needsOutput bool, fds commandFds, background bool) (pid int, output string, err error) {
	sections, err := argv.Argv(
		cmd,
		func(cmd string) (string, error) {
			return getCmdStringOutput(envs, cmdDir, cmd)
		},
		envs.expandString,
	)
	if err != nil {
		return 0, "", fmt.Errorf("parse command string failed: %s", err)
	}
	if len(sections) == 0 {
		return 0, "", fmt.Errorf("empty command line string")
	}
	return execCommand(envs, sections, cmdDir, needsOutput, fds, background)
}

func getCmdStringOutput(envs *ExpandEnvs, cmd, cmdDir string) (string, error) {
	_, output, err := runCommand(envs, cmd, cmdDir, true, commandFds{}, false)
	return output, err
}
func parseInt(s string) (int64, error) {
	for prefix, base := range map[string]int{
		"0x": 16,
		"0o": 8,
		"0b": 2,
	} {
		if strings.HasPrefix(s, prefix) {
			return strconv.ParseInt(s, base, 64)
		}
	}
	return strconv.ParseInt(s, 10, 64)
}
func parseBool(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "true", "yes", "1":
		return true, nil
	case "", "false", "no", "0":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean value: %s", s)
	}
}
func checkCondition(envs *ExpandEnvs, value, operator string, compareField *string) (bool, error) {
	fixAlias := func(o *string) {
		if a, has := syntax.OperatorAlias[*o]; has {
			*o = a
		}
	}
	if operator == "" {
		if compareField == nil {
			operator = syntax.Op_bool_true
		} else {
			operator = syntax.Op_string_equal
		}
	}
	fixAlias(&operator)

	var compare string
	if compareField != nil {
		compare = *compareField
	}
	var ok bool
	switch operator {
	case syntax.Op_string_regexp:
		r, err := regexp.CompilePOSIX(compare)
		if err != nil {
			return false, fmt.Errorf("compile regexp failed: %s, %s", compare, err)
		}
		ok = r.MatchString(value)

	case syntax.Op_string_greaterThan:
		ok = value > compare
	case syntax.Op_string_greaterThanOrEqual:
		ok = value >= compare
	case syntax.Op_string_equal:
		ok = value == compare
	case syntax.Op_string_notEqual:
		ok = value != compare
	case syntax.Op_string_lessThanOrEqual:
		ok = value <= compare
	case syntax.Op_string_lessThan:
		ok = value < compare

	case syntax.Op_number_greaterThan,
		syntax.Op_number_greaterThanOrEqual,
		syntax.Op_number_equal,
		syntax.Op_number_notEqual,
		syntax.Op_number_lessThanOrEqual,
		syntax.Op_number_lessThan:

		var (
			v1, v2     int64
			err1, err2 error
		)
		if value != "" {
			v1, err1 = parseInt(value)
		}
		if v := compare; v != "" {
			v2, err2 = parseInt(v)
		}
		if err1 != nil || err2 != nil {
			return false, fmt.Errorf("convert values to float number failed: %s, %s", value, compare)
		}
		switch operator {
		case syntax.Op_number_greaterThan:
			ok = v1 > v2
		case syntax.Op_number_greaterThanOrEqual:
			ok = v1 >= v2
		case syntax.Op_number_equal:
			ok = v1 == v2
		case syntax.Op_number_notEqual:
			ok = v1 != v2
		case syntax.Op_number_lessThanOrEqual:
			ok = v1 <= v2
		case syntax.Op_number_lessThan:
			ok = v1 < v2
		}
	case syntax.Op_file_newerThan, syntax.Op_file_olderThan:
		s1, e1 := os.Stat(value)
		s2, e2 := os.Stat(compare)
		if e1 != nil || e2 != nil {
			return false, fmt.Errorf("access files failed: %s %s", e1, e2)
		}
		switch operator {
		case syntax.Op_file_newerThan:
			ok = s1.ModTime().After(s2.ModTime())
		case syntax.Op_file_olderThan:
			ok = s1.ModTime().Before(s2.ModTime())
		}
	case syntax.Op_bool_and,
		syntax.Op_bool_or:
		o1, e1 := parseBool(value)
		o2, e2 := parseBool(compare)
		if e1 != nil || e2 != nil {
			return false, fmt.Errorf("invalid boolean value: '%s', '%s'", value, compare)
		}
		if operator == syntax.Op_bool_and {
			ok = o1 && o2
		} else {
			ok = o1 || o2
		}
	default:
		if compareField != nil {
			return false, fmt.Errorf("operator doesn't needs compare field: %s", operator)
		}

		checkFileStat := func(fn func(stat os.FileInfo) bool) bool {
			stat, err := os.Stat(value)
			return err == nil && (fn == nil || fn(stat))
		}
		checkFileStatMode := func(fn func(mode os.FileMode) bool) bool {
			return checkFileStat(func(stat os.FileInfo) bool {
				return fn(stat.Mode())
			})
		}
		checkFileLStat := func(fn func(stat os.FileInfo) bool) bool {
			stat, err := os.Lstat(value)
			return err == nil && (fn == nil || fn(stat))
		}
		checkFileLstatMode := func(fn func(mode os.FileMode) bool) bool {
			return checkFileLStat(func(stat os.FileInfo) bool {
				return fn(stat.Mode())
			})
		}
		switch operator {
		case syntax.Op_string_notEmpty:
			ok = value != ""
		case syntax.Op_string_empty:
			ok = value == ""
		case syntax.Op_bool_true, syntax.Op_bool_not:
			var err error
			ok, err = parseBool(value)
			if err != nil {
				return false, fmt.Errorf("invalid boolean value: %s", value)
			}
			if operator == syntax.Op_bool_not {
				ok = !ok
			}
		case syntax.Op_env_defined:
			ok = envs.Exist(value)
		case syntax.Op_file_exist:
			ok = checkFileStat(nil)
		case syntax.Op_file_blockDevice:
			ok = checkFileStatMode(func(mode os.FileMode) bool {
				return mode&os.ModeDevice != 0 && mode&os.ModeCharDevice == 0
			})
		case syntax.Op_file_charDevice:
			ok = checkFileStatMode(func(mode os.FileMode) bool {
				return mode&os.ModeDevice != 0 && mode&os.ModeCharDevice != 0
			})
		case syntax.Op_file_dir:
			ok = checkFileStat(func(stat os.FileInfo) bool {
				return stat.IsDir()
			})
		case syntax.Op_file_regular:
			ok = checkFileStatMode(func(mode os.FileMode) bool {
				return mode.IsRegular()
			})
		case syntax.Op_file_setgid:
			ok = checkFileStatMode(func(mode os.FileMode) bool {
				return mode&os.ModeSetgid != 0
			})
		//case "-G":
		case syntax.Op_file_symlink:
			ok = checkFileLstatMode(func(mode os.FileMode) bool {
				return mode&os.ModeSymlink != 0
			})
		case syntax.Op_file_sticky:
			ok = checkFileStatMode(func(mode os.FileMode) bool {
				return mode&os.ModeSticky != 0
			})
		//case "-N":
		//case "-O":
		case syntax.Op_file_namedPipe:
			ok = checkFileStatMode(func(mode os.FileMode) bool {
				return mode&os.ModeNamedPipe != 0
			})
		//case "-r":

		case syntax.Op_file_notEmpty:
			ok = checkFileStat(func(stat os.FileInfo) bool {
				return stat.Size() > 0
			})
		case syntax.Op_file_socket:
			ok = checkFileStatMode(func(mode os.FileMode) bool {
				return mode&os.ModeSocket != 0
			})
		//case "-t":
		case syntax.Op_file_setuid:
			ok = checkFileStatMode(func(mode os.FileMode) bool {
				return mode&os.ModeSetuid != 0
			})
		//case "-w":
		//case "-x":
		case syntax.Op_file_binary:
			_, err := exec.LookPath(value)
			if err != nil {
				if errors.Is(err, exec.ErrNotFound) {
					return false, nil
				}
				return false, fmt.Errorf("lookup executable binary failed: %w", err)
			}
			return true, nil
		default:
			return false, fmt.Errorf("invalid condition operator: %s", operator)
		}
	}
	return ok, nil
}

func fileReplacer(args []string, isRegexp bool) (func(path string) error, error) {
	if len(args) == 0 {
		return func(path string) error {
			return nil
		}, nil
	}
	withFileContent := func(fn func([]byte) []byte) func(path string) error {
		return func(path string) error {
			fd, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				return err
			}
			defer fd.Close()
			content, err := ioutil.ReadAll(fd)
			if err != nil {
				return err
			}
			content = fn(content)
			_, err = fd.Seek(0, io.SeekStart)
			if err == nil {
				err = fd.Truncate(0)
			}
			if err == nil {
				_, err = fd.Write(content)
			}
			return err
		}
	}
	if !isRegexp {
		if len(args) == 2 {
			o := []byte(args[0])
			n := []byte(args[1])
			return withFileContent(func(data []byte) []byte {
				return bytes.ReplaceAll(data, o, n)
			}), nil
		}
		r := strings.NewReplacer(args...)
		return withFileContent(func(data []byte) []byte {
			return []byte(r.Replace(string(data)))
		}), nil
	}

	type regPair struct {
		R       *regexp.Regexp
		Replace []byte
	}
	var regs []regPair
	for i := 0; i < len(args); i += 2 {
		r, err := regexp.CompilePOSIX(args[i])
		if err != nil {
			return nil, fmt.Errorf("compile regexp failed: %s, %w", args[i], err)
		}
		regs = append(regs, regPair{R: r, Replace: []byte(args[i+1])})
	}
	return withFileContent(func(data []byte) []byte {
		for _, p := range regs {
			data = p.R.ReplaceAll(data, p.Replace)
		}
		return data
	}), nil
}

func stringToSlash(s string) string {
	return filepath.ToSlash(s)
}

func ptrsToSlash(ptr ...*string) {
	for _, ptr := range ptr {
		*ptr = filepath.ToSlash(*ptr)
	}
}

func sliceToSlash(paths []string) []string {
	for i := range paths {
		paths[i] = filepath.ToSlash(paths[i])
	}
	return paths
}

func openFile(name string, append bool) (*os.File, error) {
	flags := os.O_WRONLY | os.O_CREATE
	if append {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	err := os.MkdirAll(filepath.Dir(name), 0755)
	if err != nil {
		return nil, fmt.Errorf("create parent directories failed: %w", err)
	}
	return os.OpenFile(name, flags, 00644)
}

func stringUnquote(s string) string {
	l := len(s)
	if l >= 2 {
		switch s[0] {
		case '"', '\'':
			if s[l-1] == s[0] {
				ns := s[1 : l-1]
				var valid = true
				for i := range ns {
					if ns[i] == s[0] {
						valid = false
						break
					}
				}
				if valid {
					return ns
				}
			}
		}
	}
	return s
}

func splitBlocks(s string) []string {
	var blocks []string
	arr := stringSplitAndTrimFilterSpace(s, "\n")
	for _, a := range arr {
		blocks = append(blocks, stringSplitAndTrimFilterSpace(a, ";")...)
	}
	return blocks
}

func splitBlocksAndGlobPath(path string, mustBeFile bool) ([]string, error) {
	var matched []string
	blocks := splitBlocks(path)
	for _, block := range blocks {
		m, err := zglob.Glob(block)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("glob path failed: %s, %w", block, err)
		}
		matched = append(matched, m...)
	}
	sort.Strings(matched)

	if !mustBeFile {
		return matched, nil
	}
	var end int
	for i, p := range matched {
		if mustBeFile {
			stat, err := os.Stat(p)
			if err != nil {
				continue
			}
			if stat.IsDir() {
				continue
			}
		}
		if end != i {
			matched[end] = matched[i]
		}
		end++
	}
	matched = matched[:end]
	return matched, nil
}

func runInDir(dir string, fn func() error) error {
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get current directory failed: %w", err)
	}
	err = os.Chdir(dir)
	if err != nil {
		return fmt.Errorf("chdir failed: %w", err)
	}
	err = fn()
	err2 := os.Chdir(wd)
	if err != nil {
		if err2 != nil {
			return fmt.Errorf("run failed: %w, chdir back failed: %s", err, err2)
		}
		return fmt.Errorf("run function failed: %w", err)
	}
	if err2 != nil {
		return fmt.Errorf("chdir back failed: %w", err2)
	}
	return nil
}
