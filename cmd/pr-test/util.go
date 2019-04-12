package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/user"
	"path/filepath"

	"github.com/codeskyblue/go-sh"
)

func debug(v ...interface{}) {
	if flagVerbose {
		_ = log.Output(2, fmt.Sprintln(v...))
	}
}

func debugF(format string, v ...interface{}) {
	if flagVerbose {
		_ = log.Output(2, fmt.Sprintf(format, v...))
	}
}

func getHome() (string, error) {
	home := os.Getenv("HOME")
	if home != "" {
		return home, nil
	}
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
}

func getArFiles(filename string) ([]string, error) {
	arTOut, err := sh.Command("ar", "t", filename).Output()
	if err != nil {
		return nil, err
	}

	lines := bytes.Split(arTOut, []byte("\n"))
	var files []string
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		files = append(files, string(line))
	}
	return files, nil
}

func strSliceContains(slice []string, str string) bool {
	for _, value := range slice {
		if value == str {
			return true
		}
	}
	return false
}

const markDir = "/var/lib/deepin-pr-test"

func markInstall(pkg string) error {
	_, err := os.Stat(markDir)
	if os.IsNotExist(err) {
		err = sh.Command("sudo", "mkdir", "-p", "-m", "0755", markDir).Run()
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	err = sh.Command("sudo", "touch", filepath.Join(markDir, pkg)).Run()
	return err
}

func markUninstall(pkg string) error {
	debug("markUninstall", pkg)
	filename := filepath.Join(markDir, pkg)
	_, err := os.Stat(filename)
	if err != nil {
		debug(err)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	err = sh.Command("sudo", "rm", filename).Run()
	return err
}
