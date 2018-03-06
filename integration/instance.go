// Mgmt
// Copyright (C) 2013-2018+ James Shubin and the project contributors
// Written by James Shubin <james@shubin.ca> and the project contributors
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

// notes:
// https://www.philosophicalhacker.com/post/integration-tests-in-go/
// https://github.com/golang/go/wiki/TableDrivenTests
// https://blog.golang.org/subtests
// https://splice.com/blog/lesser-known-features-go-test/
// https://coreos.com/blog/testing-distributed-systems-in-go.html
// https://www.philosophicalhacker.com/post/integration-tests-in-go/

package integration

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	errwrap "github.com/pkg/errors"
)

const (
	defaultTimeout     = 120 * time.Second // longest time a mgmt instance should exist during tests
	convergedTimeout   = 5 * time.Second
	idleTimeout        = 5 * time.Second
	shutdownTimeout    = 10 * time.Second
	convergedIndicator = "Converged for 5 seconds, exiting!"
	exitedIndicator    = "Goodbye!"
)

// Instance represents a single mgmt instance that can be managed
type Instance struct {
	// settings passed into the instance
	Timeout time.Duration
	Env     []string

	// output and metadata regarding the mgmt instance
	Name         string
	Tmpdir       string
	Prefix       string
	Workdir      string
	DeployOutput string
	Stdout       bytes.Buffer
	Stderr       bytes.Buffer
	Err          error
	Seeds        string

	command string
	env     string
	ctx     context.Context
	cmd     *exec.Cmd
	cancel  context.CancelFunc
}

// start starts running mgmt run in an isolated environment
func (m *Instance) start(mgmtargs ...string) error {
	// TODO: fakechroot/docker for proper isolation, for now relying on passing a temp. dir
	// TODO: currently better isolation for used client/server ports is much needed

	if m.Timeout == 0 {
		m.Timeout = defaultTimeout
	}

	m.ctx, m.cancel = context.WithTimeout(context.Background(), m.Timeout)

	if m.Tmpdir == "" {
		// create temporary directory to use during testing
		m.Tmpdir, err = ioutil.TempDir("", fmt.Sprintf("mgmt-integrationtest-%s-", m.Name))
		return err
	}

	prefix := path.Join(m.Tmpdir, "prefix")
	if err := os.MkdirAll(prefix, 0755); err != nil {
		return err
	}
	m.Prefix = prefix
	workdir := path.Join(m.Tmpdir, "workdir")
	if err := os.MkdirAll(workdir, 0755); err != nil {
		return err
	}
	m.Workdir = workdir

	cmdargs := []string{"run"}
	if m.Name != "" {
		cmdargs = append(cmdargs, fmt.Sprintf("--hostname=%s", m.Name))
	}
	cmdargs = append(cmdargs, mgmtargs...)
	m.command = fmt.Sprintf("%s %s", mgmt, strings.Join(cmdargs, " "))

	m.cmd = exec.CommandContext(m.ctx, mgmt, cmdargs...)

	m.cmd.Stdout = &m.Stdout
	m.cmd.Stderr = &m.Stderr

	m.cmd.Env = []string{
		fmt.Sprintf("MGMT_TEST_ROOT=%s", workdir),
		fmt.Sprintf("MGMT_PREFIX=%s", prefix),
	}
	m.cmd.Env = append(m.cmd.Env, m.Env...)
	m.env = strings.Join(m.cmd.Env, " ")

	m.Seeds = fmt.Sprintf("http://127.0.0.1:2379")

	return m.cmd.Start()
}

// wait waits for previously started mgmt run to finish cleanly
func (m *Instance) wait() error {
	return m.cmd.Wait()
}

// stop stops the mgmt background instance
func (m *Instance) stop() error {
	if err := m.cmd.Process.Signal(syscall.SIGINT); err != nil {
		return err
	}
	if err := m.cmd.Wait(); err != nil {
		// kill the process if it fails to shutdown nicely, so we get a stack trace
		m.cmd.Process.Signal(syscall.SIGKILL)
		return err
	}
	if m.ctx.Err() == context.DeadlineExceeded {
		return m.ctx.Err()
	}
	return nil
}

// Run runs mgmt to convergence
func (m *Instance) Run(mgmtargs ...string) error {
	mgmtargs = append(mgmtargs, "--converged-timeout=5")
	if err := m.start(mgmtargs...); err != nil {
		return err
	}
	return m.wait()
}

// RunLangFile runs mgmt with the given mcl file to convergence
func (m *Instance) RunLangFile(langfilerelpath string, mgmtargs ...string) error {
	_, testfilepath, _, _ := runtime.Caller(0)
	testdirpath := filepath.Dir(testfilepath)
	langfilepath := path.Join(testdirpath, langfilerelpath)

	mgmtargs = append(mgmtargs, fmt.Sprintf("--lang=%s", langfilepath))
	return m.Run(mgmtargs...)
}

// RunBackground runs mgmt in the background
func (m *Instance) RunBackground(mgmtargs ...string) error {
	return m.start(mgmtargs...)
}

// DeployLang deploys a mcl file provided as content to the running instance
func (m *Instance) DeployLang(code string) (string, error) {
	tmpfile, err := ioutil.TempFile("", "deploy.mcl")
	if err != nil {
		return "", err
	}

	defer os.Remove(tmpfile.Name()) // clean up

	if _, err := tmpfile.Write([]byte(code)); err != nil {
		return "", err
	}
	if err := tmpfile.Close(); err != nil {
		return "", err
	}

	cmd := exec.Command(mgmt, "deploy", "--no-git", "lang", "--lang", tmpfile.Name())
	// TODO: environment should be shared by instance and deploy
	cmd.Env = []string{fmt.Sprintf("MGMT_SEEDS=%s", m.Seeds)}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), errwrap.Wrapf(err, "Deploy failed")
	}
	m.DeployOutput = string(out)
	return string(out), nil
}

// DeployLangFile deploys a mcl file to the running instance
func (m *Instance) DeployLangFile(langfilerelpath string) (string, error) {
	_, testfilepath, _, _ := runtime.Caller(0)
	testdirpath := filepath.Dir(testfilepath)
	langfilepath := path.Join(testdirpath, langfilerelpath)

	cmd := exec.Command(mgmt, "deploy", "--no-git", "lang", "--lang", langfilepath)
	// TODO: environment should be shared by instance and deploy
	cmd.Env = []string{fmt.Sprintf("MGMT_SEEDS=%s", m.Seeds)}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), errwrap.Wrapf(err, "Deploy failed")
	}
	m.DeployOutput = string(out)
	return string(out), nil
}

// StopBackground stops the mgmt instance running in the background
func (m *Instance) StopBackground() error {
	err := m.stop()
	// TODO: proper shutdown checking, eg "Main: Waiting..." is last logline for x seconds
	time.Sleep(shutdownTimeout)
	return err
}

// Finished indicates if the command output matches that of a converged and finished (exited) run
func (m Instance) Finished(converge bool) error {
	if m.Stderr.String() == "" {
		return fmt.Errorf("Instance run had no output")
	}
	var converged bool
	if converge {
		converged = strings.Contains(m.Stderr.String(), convergedIndicator)
	} else {
		converged = true
	}
	exited := strings.Contains(m.Stderr.String(), exitedIndicator)
	if !(converged && exited) {
		return fmt.Errorf("Instance run output does not indicate finished run")
	}
	return nil
}

// Pass checks if a non-empty `pass` file exists in the workdir
// This file should be created by the configuration run to indicate it completed.
func (m Instance) Pass() error {
	passfilepath := path.Join(m.Workdir, "pass")
	passfilestat, err := os.Stat(passfilepath)
	if os.IsNotExist(err) {
		return fmt.Errorf("the file `%s` was not created by the configuration", passfilepath)
	}
	if passfilestat.Size() == 0 {
		return fmt.Errorf("the file `%s` is empty", passfilepath)
	}
	return nil
}

// Cleanup makes sure temporary directory is cleaned
func (m *Instance) Cleanup() error {
	// stop the timeout context for the command
	m.cancel()
	// we expect the command to be stopped (StopBackground()) or else assume we don't care about it closing nicely
	m.cmd.Process.Signal(syscall.SIGKILL)

	if m.Tmpdir == "" {
		return nil
	}
	if err := os.RemoveAll(m.Tmpdir); err != nil {
		return err
	}
	m.Tmpdir = ""
	return nil
}

// WaitUntilIdle waits for the current instance to reach reach idle state ()
func (m *Instance) WaitUntilIdle() error {
	// TODO: sleep is bad UX on testing, refactor to wait for a signal either from logging or maybe using etcdclient and AddHostnameConvergedWatcher?
	// TODO: should we consider being idle the same as being converged?
	time.Sleep(idleTimeout)
	return nil
}

// WaitUntilConverged waits for the current instance to reach a converged state
func (m *Instance) WaitUntilConverged() error {
	// TODO: sleep is bad UX on testing, refactor to wait for a signal either from logging or maybe using etcdclient and AddHostnameConvergedWatcher?
	time.Sleep(convergedTimeout)
	return nil
}

// WorkdirWriteToFile write a string to a file in the current instance workdir
func (m *Instance) WorkdirWriteToFile(name string, text string) error {
	data := []byte(text)
	return ioutil.WriteFile(path.Join(m.Workdir, name), data, 0644)
}

// WorkdirReadFromFile reads a file from the workdir and returns the content as a string
func (m *Instance) WorkdirReadFromFile(name string) (string, error) {
	data, err := ioutil.ReadFile(path.Join(m.Workdir, name))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// WorkdirRemoveFile removes a file from the instance workdir
func (m *Instance) WorkdirRemoveFile(name string) error {
	return os.Remove(path.Join(m.Workdir, name))
}