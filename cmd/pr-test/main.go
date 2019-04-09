package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/codeskyblue/go-sh"
	"github.com/davecgh/go-spew/spew"
	"github.com/google/go-github/github"
	"github.com/levigross/grequests"
	"golang.org/x/oauth2"
	"pault.ag/go/debian/control"
)

var flagStatus bool

func init() {
	flag.BoolVar(&flagStatus, "status", false, "")
}

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

func installDeb(debUrl *url.URL, pkgName string) error {
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

	modifiedFilename, err := modifyDeb(filename)
	if err != nil {
		return err
	}

	err = sh.Command("sudo", "apt", "install", "-y",
		"--allow-downgrades", "--reinstall", modifiedFilename).Run()
	if err != nil {
		return err
	}

	err = markInstall(pkgName)
	return err
}

func saveDeb() {

}

func modifyDeb(filename string) (modifiedFilename string, err error) {
	modifiedFilename = filepath.Join(tempDebModifiedDir, filepath.Base(filename))
	log.Println("modifiedFilename:", modifiedFilename)

	err = os.MkdirAll(tempDebModifiedDir, 0755)
	if err != nil {
		return
	}

	err = sh.Command("cp", filename, modifiedFilename).Run()
	if err != nil {
		return
	}

	tempDir, err := ioutil.TempDir("", "pr-test-mod")
	if err != nil {
		return
	}
	log.Println("tempDir:", tempDir)
	defer func() {
		err := os.RemoveAll(tempDir)
		if err != nil {
			log.Println("WARN:", err)
		}
	}()

	arFiles, err := getArFiles(modifiedFilename)
	if err != nil {
		return
	}

	var controlTarFile string
	var tarExt string
	for _, file := range arFiles {
		if strings.HasPrefix(file, "control.tar") {
			controlTarFile = file
			tarExt = filepath.Ext(file)
		}
	}

	if controlTarFile == "" {
		err = errors.New("not found control tar file in deb file")
		return
	}

	session := sh.NewSession().SetDir(tempDir)
	err = session.Command("ar", "x", modifiedFilename, controlTarFile).Run()
	if err != nil {
		return
	}

	switch tarExt {
	case ".gz":
		err = session.Command("gunzip", controlTarFile).Run()
	case ".xz":
		err = session.Command("xz", "-d", controlTarFile).Run()
	default:
		err = fmt.Errorf("unknown control.tar ext %q", tarExt)
	}
	if err != nil {
		return
	}

	err = session.Command("tar", "--extract", "--file=control.tar", "./control").Run()
	if err != nil {
		return
	}

	err = modifyControl(filepath.Join(tempDir, "control"))
	if err != nil {
		return
	}

	// rebuild deb
	err = session.Command("tar", "--update", "-f", "control.tar", "./control").Run()
	if err != nil {
		return
	}

	switch tarExt {
	case ".gz":
		err = session.Command("gzip", "control.tar").Run()
	case ".xz":
		err = session.Command("xz", "-z", "control.tar").Run()
	}
	if err != nil {
		return
	}

	err = session.Command("ar", "r", modifiedFilename, controlTarFile).Run()
	if err != nil {
		return
	}

	return
}

func modifyControl(filename string) error {
	ctl, err := control.ParseControlFile(filename)
	if err != nil {
		return err
	}

	spew.Dump(ctl)

	// TODO
	//ctl.Source.Description

	return nil
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

func main() {
	log.SetFlags(log.Lshortfile | log.LstdFlags)
	flag.Parse()

	if flagStatus {
		showStatus()
		return
	}

	token, err := getGithubAccessToken()
	if err != nil {
		log.Println("WARN: failed to get github access token:", err)
	}

	ctx := context.Background()
	var httpClient *http.Client
	if token != "" {
		httpClient = oauth2.NewClient(ctx, oauth2.StaticTokenSource(
			&oauth2.Token{
				AccessToken: token,
			}))
	}
	client := github.NewClient(httpClient)

	prId, err := getPRIdFromCmdArg(flag.Arg(0))
	if err != nil {
		log.Fatal(err)
	}

	err = installPullRequest(client, prId)
	if err != nil {
		log.Fatal(err)
	}
}

type pullRequestId struct {
	repo string
	num  int
}

func getPRIdFromCmdArg(arg string) (pullRequestId, error) {
	num, err := strconv.Atoi(arg)
	if err == nil {
		repo, err := getRepoFromGitConfig()
		if err != nil {
			return pullRequestId{}, err
		}
		return pullRequestId{repo: repo, num: num}, nil
	}
	reg := regexp.MustCompile(`^(\S+)#(\d+)$`)
	match := reg.FindStringSubmatch(arg)
	if match != nil {
		repo := match[1]
		num, err := strconv.Atoi(match[2])
		if err != nil {
			return pullRequestId{}, err
		}
		return pullRequestId{repo: repo, num: num}, nil
	}

	return parsePullUrl(arg)
}

func getRepoFromGitConfig() (string, error) {
	out, err := sh.Command("git", "config", "--local",
		"--get-regexp", `remote\..*\.url`).Output()
	if err != nil {
		return "", err
	}
	remotes := bytes.Split(out, []byte("\n"))
	reg := regexp.MustCompile(fmt.Sprintf(`github.com[:/]%s/(.+)$`, organization))
	for _, remote := range remotes {
		match := reg.FindSubmatch(remote)
		if match != nil {
			repo := string(match[1])
			repo = strings.TrimSuffix(repo, ".git")
			return repo, nil
		}
	}
	return "", errors.New("repo not found in remote urls")
}

func getSuccessStatus(statuses []*github.RepoStatus) *github.RepoStatus {
	for _, status := range statuses {
		if status.GetState() == "success" {
			return status
		}
	}
	return nil
}

func parsePullUrl(pullUrl string) (prId pullRequestId, err error) {
	reg := regexp.MustCompile("https://github.com/" + organization + `/([^/]+)/pull/(\d+)`)
	match := reg.FindStringSubmatch(pullUrl)
	if match == nil {
		err = errors.New("invalid pull url")
		return
	}

	prId.repo = match[1]
	prId.num, err = strconv.Atoi(match[2])
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

func showPullRequestInfo(pr *github.PullRequest) {
	title := pr.GetTitle()
	state := pr.GetState()
	user := pr.GetUser().GetLogin()

	log.Println("title:", title)
	log.Println("state:", state)
	log.Println("user:", user)
}

func installPullRequest(client *github.Client, prId pullRequestId) error {
	ctx := context.Background()
	log.Printf("repo: %v, num: %v\n", prId.repo, prId.num)

	pr, _, err := client.PullRequests.Get(ctx, organization, prId.repo, prId.num)
	if err != nil {
		return err
	}

	showPullRequestInfo(pr)

	prRef := pr.GetHead().GetSHA()
	if prRef == "" {
		return errors.New("failed to get pull request ref")
	}
	statuses, _, err := client.Repositories.ListStatuses(ctx, organization, prId.repo,
		prRef, nil)
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

		errMsg := "not found success status"
		if targetUrl0 != "" {
			errMsg += ", please see " + targetUrl0
		}
		return errors.New(errMsg)
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
			err = installDeb(debUrl, pkgName)
			if err != nil {
				return err
			}
		}

	}
	return nil
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
	// TODO
	return nil
}

func showStatus() error {
	fileInfos, err := ioutil.ReadDir(markDir)
	if err != nil {
		return err
	}
	for _, fileInfo := range fileInfos {
		log.Println(fileInfo.Name())
	}
	return nil
}
