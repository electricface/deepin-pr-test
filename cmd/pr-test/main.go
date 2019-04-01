package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/codeskyblue/go-sh"
	"github.com/google/go-github/github"
	"github.com/levigross/grequests"
	"golang.org/x/oauth2"
)

const (
	organization = "linuxdeepin"
)

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

func test() {
	//jobUrl := "https://ci.deepin.io/job/dde-file-manager/5981/"
	os.Exit(1)
}

const (
	tempDebDownloadDir = "/tmp/pr-test/deb_download"
	tempDebModifiedDir = "/tmp/pr-test/deb_modified"
)

func getUrlBasename(u *url.URL) (string, error) {
	p, err := url.PathUnescape(u.Path)
	if err != nil {
		return "", err
	}
	base := path.Base(p)
	return base, nil
}

func installDeb(debUrl *url.URL) error {
	u := debUrl.String()
	log.Println("download from", u)
	resp, err := grequests.Get(u, nil)
	if err != nil {
		return err
	}

	base, err := getUrlBasename(debUrl)
	if err != nil {
		return err
	}
	filename := filepath.Join(tempDebDownloadDir, base)

	err = os.MkdirAll(tempDebDownloadDir, 0755)
	if err != nil {
		return err
	}

	err = resp.DownloadToFile(filename)
	if err != nil {
		return err
	}

	err = sh.Command("sudo", "apt", "install", "-y", "--allow-downgrades", filename).Run()
	if err != nil {
		return err
	}

	return nil
}

func saveDeb() {

}

func modifyDeb() {

}

func main() {
	log.SetFlags(log.Lshortfile | log.LstdFlags)
	//test()
	flag.Parse()
	token, err := getGithubAccessToken()
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	tc := oauth2.NewClient(ctx, oauth2.StaticTokenSource(
		&oauth2.Token{
			AccessToken: token,
		}))

	client := github.NewClient(tc)

	pullUrl := flag.Arg(0)
	err = installPR(client, pullUrl)
	if err != nil {
		log.Fatal(err)
	}
}

func getSuccessStatus(statuses []*github.RepoStatus) *github.RepoStatus {
	for _, status := range statuses {
		if status.GetState() == "success" {
			return status
		}
	}
	return nil
}

func parsePullUrl(pullUrl string) (repo string, prNum int, err error) {
	reg := regexp.MustCompile("https://github.com/" + organization + `/([^/]+)/pull/(\d+)`)
	match := reg.FindStringSubmatch(pullUrl)
	if match == nil {
		err = errors.New("invalid pull url")
		return
	}

	repo = match[1]
	prNum, err = strconv.Atoi(match[2])
	return
}

var regHrefDeb1 = regexp.MustCompile(`href="(\S+\.deb)">`)
var regHrefDeb2 = regexp.MustCompile(`\.href = '(\S+\.deb)'`)

func getDebUrls(jobUrl string) ([]*url.URL, error) {
	resp, err := grequests.Get(jobUrl, nil)
	if err != nil {
		return nil, err
	}

	if !strings.HasSuffix(jobUrl, "/") {
		jobUrl += "/"
	}

	respStr := resp.String()
	allMatch := regHrefDeb1.FindAllStringSubmatch(respStr, -1)
	if allMatch == nil {
		// deb urls are collapsed, try another regex
		allMatch = regHrefDeb2.FindAllStringSubmatch(respStr, -1)
	}

	if allMatch == nil {
		return nil, nil
	}

	var result = make([]*url.URL, len(allMatch))
	for idx, match := range allMatch {
		u, err := url.Parse(jobUrl + match[1])
		if err != nil {
			return nil, err
		}
		result[idx] = u
	}
	return result, nil
}

func askYesNo(prompt string, defaultYes bool) (yes bool, err error) {
	var suffix string
	if defaultYes {
		suffix = " (Yes/n) "
	} else {
		suffix = " (y/No) "
	}
	fmt.Print(prompt, suffix)
	var input string
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		input = scanner.Text()
		input = strings.TrimSpace(input)
	}
	if scanner.Err() != nil {
		return false, scanner.Err()
	}
	if input == "" {
		return defaultYes, nil
	}
	return strings.HasPrefix(input, "y") ||
		strings.HasPrefix(input, "Y"), nil
}

func parseDebFilename(filename string) (pkgName, version, arch string, err error) {
	filename = strings.TrimSuffix(filename, ".deb")
	fields := strings.SplitN(filename, "_", 3)
	if len(fields) != 3 {
		err = errors.New("parseDebFilename: len fields != 3")
		return
	}
	pkgName = fields[0]
	version = fields[1]
	arch = fields[2]
	return
}

func installPR(client *github.Client, pullUrl string) error {
	ctx := context.Background()
	repo, prNum, err := parsePullUrl(pullUrl)
	if err != nil {
		return err
	}
	log.Println(repo, prNum)

	pr, _, err := client.PullRequests.Get(ctx, organization, repo, prNum)
	if err != nil {
		return err
	}

	prRef := pr.GetHead().GetSHA()
	if prRef == "" {
		return errors.New("failed to get pull request ref")
	}
	statuses, _, err := client.Repositories.ListStatuses(ctx, organization, repo, prRef, nil)
	if err != nil {
		return err
	}

	status := getSuccessStatus(statuses)
	if status == nil {
		var targetUrl0 string
		for _, status := range statuses {
			targetUrl := status.GetTargetURL()
			if targetUrl != "" {
				targetUrl0 = targetUrl
			}
			break
		}

		return errors.New("not found success status, please see " + targetUrl0)
	}

	targetUrl := status.GetTargetURL()
	if targetUrl == "" {
		return errors.New("target url is empty")
	}

	log.Println("targetUrl:", targetUrl)

	err = installJobDebs(strings.TrimSuffix(targetUrl, "/console"))
	return err
}

func installJobDebs(jobUrl string) error {
	debUrls, err := getDebUrls(jobUrl)
	if err != nil {
		return err
	}

	for _, debUrl := range debUrls {
		base, err := getUrlBasename(debUrl)
		if err != nil {
			return err
		}

		pkgName, _, _, err := parseDebFilename(base)
		if err != nil {
			return err
		}

		defaultYes := true
		if strings.HasSuffix(pkgName, "-dev") ||
			strings.HasSuffix(pkgName, "-dbg") ||
			strings.HasSuffix(pkgName, "-dbgsym") {
			defaultYes = false
		}

		respYes, err := askYesNo(fmt.Sprintf("install %s?", pkgName), defaultYes)
		if err != nil {
			return err
		}

		if respYes {
			err = installDeb(debUrl)
			if err != nil {
				return err
			}
		}

	}
	return nil
}
