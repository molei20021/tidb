// Copyright 2021 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strings"
	"sync"
	"time"

	// Set the correct when it runs inside docker.
	_ "go.uber.org/automaxprocs"
)

func usage() bool {
	msg := `// run all tests
ut

// show usage
ut -h

// list all packages
ut list

// list test cases of a single package
ut list $package

// run all tests
ut run

// run test all cases of a single package
ut run $package

// run test cases of a single package
ut run $package $test

// build all test package
ut build

// build a test package
ut build xxx`
	fmt.Println(msg)
	return true
}

const modulePath = "github.com/pingcap/tidb"

type task struct {
	pkg  string
	test string
	old  bool
}

var P int
var workDir string

func cmdList(args ...string) bool {
	pkgs, err := listPackages()
	if err != nil {
		fmt.Println("list package error", err)
		return false
	}

	// list all packages
	if len(args) == 0 {
		for _, pkg := range pkgs {
			fmt.Println(pkg)
		}
		return false
	}

	// list test case of a single package
	if len(args) == 1 {
		pkg := args[0]
		pkgs = filter(pkgs, func(s string) bool { return s == pkg })
		if len(pkgs) != 1 {
			fmt.Println("package not exist", pkg)
			return false
		}

		err := buildTestBinary(pkg)
		if err != nil {
			fmt.Println("build package error", pkg, err)
			return false
		}
		exist, err := testBinaryExist(pkg)
		if err != nil {
			fmt.Println("check test binary existance error", err)
			return false
		}
		if !exist {
			fmt.Println("no test case in ", pkg)
			return false
		}

		res, err := listTestCases(pkg, nil)
		if err != nil {
			fmt.Println("list test cases for package error", err)
			return false
		}
		for _, x := range res {
			fmt.Println(x.test)
		}
	}
	return true
}

func cmdBuild(args ...string) bool {
	pkgs, err := listPackages()
	if err != nil {
		fmt.Println("list package error", err)
		return false
	}

	// build all packages
	if len(args) == 0 {
		err := buildTestBinaryMulti(pkgs)
		if err != nil {
			fmt.Println("build package error", pkgs, err)
			return false
		}
		return true
	}

	// build test binary of a single package
	if len(args) >= 1 {
		pkg := args[0]
		err := buildTestBinary(pkg)
		if err != nil {
			fmt.Println("build package error", pkg, err)
			return false
		}
	}
	return true
}

func cmdRun(args ...string) bool {
	var err error
	pkgs, err := listPackages()
	if err != nil {
		fmt.Println("list packages error", err)
		return false
	}
	tasks := make([]task, 0, 5000)
	start := time.Now()
	// run all tests
	if len(args) == 0 {
		err := buildTestBinaryMulti(pkgs)
		if err != nil {
			fmt.Println("build package error", pkgs, err)
			return false
		}

		for _, pkg := range pkgs {
			exist, err := testBinaryExist(pkg)
			if err != nil {
				fmt.Println("check test binary existance error", err)
				return false
			}
			if !exist {
				fmt.Println("no test case in ", pkg)
				continue
			}

			tasks, err = listTestCases(pkg, tasks)
			if err != nil {
				fmt.Println("list test cases error", err)
				return false
			}
		}
	}

	// run tests for a single package
	if len(args) == 1 {
		pkg := args[0]
		err := buildTestBinary(pkg)
		if err != nil {
			fmt.Println("build package error", pkg, err)
			return false
		}
		exist, err := testBinaryExist(pkg)
		if err != nil {
			fmt.Println("check test binary existance error", err)
			return false
		}

		if !exist {
			fmt.Println("no test case in ", pkg)
			return false
		}
		tasks, err = listTestCases(pkg, tasks)
		if err != nil {
			fmt.Println("list test cases error", err)
			return false
		}
	}

	// run a single test
	if len(args) == 2 {
		pkg := args[0]
		err := buildTestBinary(pkg)
		if err != nil {
			fmt.Println("build package error", pkg, err)
			return false
		}
		exist, err := testBinaryExist(pkg)
		if err != nil {
			fmt.Println("check test binary existance error", err)
			return false
		}
		if !exist {
			fmt.Println("no test case in ", pkg)
			return false
		}

		tasks, err = listTestCases(pkg, tasks)
		if err != nil {
			fmt.Println("list test cases error", err)
			return false
		}
		// filter the test case to run
		tmp := tasks[:0]
		for _, task := range tasks {
			if strings.Contains(task.test, args[1]) {
				tmp = append(tmp, task)
			}
		}
		tasks = tmp
	}
	fmt.Printf("building task finish, count=%d, takes=%v\n", len(tasks), time.Since(start))

	taskCh := make(chan task, 100)
	works := make([]numa, P)
	var wg sync.WaitGroup
	for i := 0; i < P; i++ {
		wg.Add(1)
		go works[i].worker(&wg, taskCh)
	}

	shuffle(tasks)
	for _, task := range tasks {
		taskCh <- task
	}
	close(taskCh)
	wg.Wait()
	for _, work := range works {
		if work.Fail {
			return false
		}
	}
	if coverprofile != "" {
		collectCoverProfileFile()
	}
	return true
}

// handleFlags strip the '--flag xxx' from the command line os.Args
// Example of the os.Args changes
// Before: ut run sessoin TestXXX --coverprofile xxx --junitfile yyy
// After: ut run session TestXXX
// The value of the flag is returned.
func handleFlags(flag string) string {
	var res string
	tmp := os.Args[:0]
	// Iter to the flag
	var i int
	for ; i < len(os.Args); i++ {
		if os.Args[i] == flag {
			i++
			break
		}
		tmp = append(tmp, os.Args[i])
	}
	// Handle the flag
	if i < len(os.Args) {
		res = os.Args[i]
		i++
	}
	// Iter the remain flags
	for ; i < len(os.Args); i++ {
		tmp = append(tmp, os.Args[i])
	}

	// os.Args is now the original flags with '--coverprofile XXX' removed.
	os.Args = tmp
	return res
}

var coverprofile string
var coverFileTempDir string

func main() {
	coverprofile = handleFlags("--coverprofile")
	if coverprofile != "" {
		var err error
		coverFileTempDir, err = os.MkdirTemp(os.TempDir(), "cov")
		if err != nil {
			fmt.Println("create temp dir fail", coverFileTempDir)
			os.Exit(1)
		}
		defer os.Remove(coverFileTempDir)
	}

	// Get the correct count of CPU if it's in docker.
	P = runtime.GOMAXPROCS(0)
	rand.Seed(time.Now().Unix())
	var err error
	workDir, err = os.Getwd()
	if err != nil {
		fmt.Println("os.Getwd() error", err)
	}

	if len(os.Args) == 1 {
		// run all tests
		cmdRun()
		return
	}

	if len(os.Args) >= 2 {
		var isSucceed bool
		switch os.Args[1] {
		case "list":
			isSucceed = cmdList(os.Args[2:]...)
		case "build":
			isSucceed = cmdBuild(os.Args[2:]...)
		case "run":
			isSucceed = cmdRun(os.Args[2:]...)
		default:
			isSucceed = usage()
		}
		if !isSucceed {
			os.Exit(1)
		}
	}
}

func collectCoverProfileFile() {
	// Combine all the cover file of single test function into a whole.
	files, err := os.ReadDir(coverFileTempDir)
	if err != nil {
		fmt.Println("collect cover file error:", err)
		os.Exit(-1)
	}

	w, err := os.Create(coverprofile)
	if err != nil {
		fmt.Println("create cover file error:", err)
		os.Exit(-1)
	}
	defer w.Close()
	w.WriteString("mode: set\n")

	for _, file := range files {
		if file.IsDir() {
			continue
		}

		f, err := os.Open(path.Join(coverFileTempDir, file.Name()))
		if err != nil {
			fmt.Println("open temp cover file error:", err)
			os.Exit(-1)
		}
		defer f.Close()

		r := bufio.NewReader(f)
		line, _, err := r.ReadLine()
		if err != nil || string(line) != "mode: set" {
			continue
		}

		io.Copy(w, r)
	}
}

func listTestCases(pkg string, tasks []task) ([]task, error) {
	newCases, err := listNewTestCases(pkg)
	if err != nil {
		fmt.Println("list test case error", pkg, err)
		return nil, withTrace(err)
	}
	for _, c := range newCases {
		tasks = append(tasks, task{pkg, c, false})
	}

	oldCases, err := listOldTestCases(pkg)
	if err != nil {
		fmt.Println("list old test case error", pkg, err)
		return nil, withTrace(err)
	}
	for _, c := range oldCases {
		tasks = append(tasks, task{pkg, c, true})
	}
	return tasks, nil
}

func listPackages() ([]string, error) {
	cmd := exec.Command("go", "list", "./...")
	ss, err := cmdToLines(cmd)
	if err != nil {
		return nil, withTrace(err)
	}

	ret := ss[:0]
	for _, s := range ss {
		if !strings.HasPrefix(s, modulePath) {
			continue
		}
		pkg := s[len(modulePath)+1:]
		if skipDIR(pkg) {
			continue
		}
		ret = append(ret, pkg)
	}
	return ret, nil
}

type numa struct {
	Fail    bool
}

func (n *numa) worker(wg *sync.WaitGroup, ch chan task) {
	defer wg.Done()
	for t := range ch {
		start := time.Now()
		res := n.runTestCase(t.pkg, t.test, t.old)
		if res.err != nil {
			fmt.Println("[FAIL] ", t.pkg, t.test, t.old, time.Since(start), res.err)
			io.Copy(os.Stderr, &res.output)
			n.Fail = true
		}
	}
}

type testResult struct {
	err    error
	output bytes.Buffer
}

func (n *numa) runTestCase(pkg string, fn string, old bool) (res testResult) {
	exe := "./" + testFileName(pkg)
	cmd := n.testCommand(exe, fn, old)
	cmd.Dir = path.Join(workDir, pkg)
	// Combine the test case output, so the run result for failed cases can be displayed.
	cmd.Stdout = &res.output
	cmd.Stderr = &res.output
	if err := cmd.Run(); err != nil {
		res.err = withTrace(err)
	}
	return res
}

func (n *numa) testCommand(exe string, fn string, old bool) *exec.Cmd {
	if old {
		// session.test -test.run '^TestT$' -check.f testTxnStateSerialSuite.TestTxnInfoWithPSProtoco
		args = append(args, "-test.run", "^TestT$", "-check.f", fn)
	} else {
		// session.test -test.run TestClusteredPrefixColum
		args = append(args, "-test.run", fn)
	}

	return exec.Command(exe, args...)
}

func skipDIR(pkg string) bool {
	skipDir := []string{"br", "cmd", "dumpling"}
	for _, ignore := range skipDir {
		if strings.HasPrefix(pkg, ignore) {
			return true
		}
	}
	return false
}

func buildTestBinary(pkg string) error {
	// go test -c
	var cmd *exec.Cmd
	if coverprofile != "" {
		cmd = exec.Command("go", "test", "-c", "-cover", "-vet", "off", "-o", testFileName(pkg))
	} else {
		cmd = exec.Command("go", "test", "-c", "-vet", "off", "-o", testFileName(pkg))
	}
	cmd.Dir = path.Join(workDir, pkg)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return withTrace(err)
	}
	return nil
}

// buildTestBinaryMulti is much faster than build the test packages one by one.
func buildTestBinaryMulti(pkgs []string) error {
       // go test --exec=xprog -cover -vet=off --count=0 $(pkgs)
       xprogPath := path.Join(workDir, "tools/bin/xprog")
       packages := make([]string, 0, len(pkgs))
       for _, pkg := range pkgs {
               packages = append(packages, path.Join(modulePath, pkg))
       }

       var cmd *exec.Cmd
	cmd = exec.Command("go", "test", "--exec", xprogPath, "-vet", "off", "-count", "0")
       cmd.Args = append(cmd.Args, packages...)
       cmd.Dir = workDir
       cmd.Stdout = os.Stdout
       cmd.Stderr = os.Stderr
       if err := cmd.Run(); err != nil {
               return withTrace(err)
       }
       return nil
}

func testBinaryExist(pkg string) (bool, error) {
	_, err := os.Stat(testFileFullPath(pkg))
	if err != nil {
		if _, ok := err.(*os.PathError); ok {
			return false, nil
		}
	}
	return true, withTrace(err)
}
func testFileName(pkg string) string {
	_, file := path.Split(pkg)
	return file + ".test.bin"
}

func testFileFullPath(pkg string) string {
	return path.Join(workDir, pkg, testFileName(pkg))
}

func listNewTestCases(pkg string) ([]string, error) {
	exe := "./" + testFileName(pkg)

	// session.test -test.list Test
	cmd := exec.Command(exe, "-test.list", "Test")
	cmd.Dir = path.Join(workDir, pkg)
	res, err := cmdToLines(cmd)
	if err != nil {
		return nil, withTrace(err)
	}
	return filter(res, func(s string) bool {
		return strings.HasPrefix(s, "Test") && s != "TestT" && s != "TestBenchDaily"
	}), nil
}

func listOldTestCases(pkg string) (res []string, err error) {
	exe := "./" + testFileName(pkg)

	// Maybe the restructure is finish on this package.
	cmd := exec.Command(exe, "-h")
	cmd.Dir = path.Join(workDir, pkg)
	buf, err := cmd.CombinedOutput()
	if err != nil {
		err = withTrace(err)
		return
	}
	if !bytes.Contains(buf, []byte("check.list")) {
		// there is no old test case in pkg
		return
	}

	// session.test -test.run TestT -check.list Test
	cmd = exec.Command(exe, "-test.run", "^TestT$", "-check.list", "Test")
	cmd.Dir = path.Join(workDir, pkg)
	res, err = cmdToLines(cmd)
	res = filter(res, func(s string) bool { return strings.Contains(s, "Test") })
	return res, withTrace(err)
}

func cmdToLines(cmd *exec.Cmd) ([]string, error) {
	res, err := cmd.Output()
	if err != nil {
		return nil, withTrace(err)
	}
	ss := bytes.Split(res, []byte{'\n'})
	ret := make([]string, len(ss))
	for i, s := range ss {
		ret[i] = string(s)
	}
	return ret, nil
}

func filter(input []string, f func(string) bool) []string {
	ret := input[:0]
	for _, s := range input {
		if f(s) {
			ret = append(ret, s)
		}
	}
	return ret
}

func shuffle(tasks []task) {
	for i := 0; i < len(tasks); i++ {
		pos := rand.Intn(len(tasks))
		tasks[i], tasks[pos] = tasks[pos], tasks[i]
	}
}

type errWithStack struct {
	err error
	buf []byte
}

func (e *errWithStack) Error() string {
	return e.err.Error() + "\n" + string(e.buf)
}

func withTrace(err error) error {
	if err == nil {
		return err
	}
	if _, ok := err.(*errWithStack); ok {
		return err
	}
	var stack [4096]byte
	sz := runtime.Stack(stack[:], false)
	return &errWithStack{err, stack[:sz]}
}
