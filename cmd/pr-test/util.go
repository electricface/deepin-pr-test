package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/user"

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
