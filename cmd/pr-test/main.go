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
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/codeskyblue/go-sh"
	"github.com/google/go-github/github"
	"github.com/levigross/grequests"
	"golang.org/x/oauth2"
	"pault.ag/go/debian/control"
)

var flagStatus bool
var flagVerbose bool
var flagRestore string

func init() {
	flag.BoolVar(&flagStatus, "status", false, "")
	flag.BoolVar(&flagVerbose, "verbose", false, "")
	flag.StringVar(&flagRestore, "restore", "", "all|$repo|$user")
}

const (
	organization = "linuxdeepin"

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

func installDeb(debUrl *url.URL, pkgName string, detail *jobDetail) error {
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

	modifiedFilename, err := modifyDeb(filename, &debDetail{
		url:       u,
		jobDetail: detail,
	})
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

func modifyDeb(filename string, detail *debDetail) (modifiedFilename string, err error) {
	modifiedFilename = filepath.Join(tempDebModifiedDir, filepath.Base(filename))
	logDebugln("modifiedFilename:", modifiedFilename)

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
	logDebugln("tempDir:", tempDir)
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

	const (
		extGz = ".gz"
		extXz = ".xz"
	)
	switch tarExt {
	case extGz:
		err = session.Command("gunzip", controlTarFile).Run()
	case extXz:
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

	err = modifyControl(filepath.Join(tempDir, "control"), detail)
	if err != nil {
		return
	}

	// rebuild deb
	err = session.Command("tar", "--update", "-f", "control.tar", "./control").Run()
	if err != nil {
		return
	}

	switch tarExt {
	case extGz:
		err = session.Command("gzip", "control.tar").Run()
	case extXz:
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

func modifyControl(filename string, detail *debDetail) error {
	ctl, err := control.ParseControlFile(filename)
	if err != nil {
		return err
	}

	srcParagraph := ctl.Source

	var descBuf bytes.Buffer
	descBuf.WriteString(srcParagraph.Description)
	descBuf.WriteString("The following information is added by deepin-pr-test\n=begin\n")
	prDetail := detail.jobDetail.prDetail
	parts := []string{
		"PR_URL", prDetail.url,
		"PR_REPO", prDetail.repo,
		"PR_NUM", strconv.Itoa(prDetail.num),
		"PR_USER", prDetail.user,
		"PR_TITLE", prDetail.title,
		"PR_STATE", prDetail.state,

		"CI_URL", detail.jobDetail.url,

		"DEB_URL", detail.url,
		"DEB_MODIFY_TIME", time.Now().Format(time.RFC3339),
	}
	for i := 0; i < len(parts); i += 2 {
		descBuf.WriteString(parts[i])
		descBuf.WriteByte('=')
		descBuf.WriteString(parts[i+1])
		descBuf.WriteByte('\n')
	}
	// NOTE: paragraph 的 Description 不能以 \n 结尾。
	descBuf.WriteString("=end")
	srcParagraph.Set("Description", descBuf.String())

	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer func() {
		err = f.Close()
		if err != nil {
			log.Println(err)
		}
	}()

	bw := bufio.NewWriter(f)
	err = srcParagraph.WriteTo(bw)
	if err != nil {
		return err
	}
	err = bw.Flush()
	if err != nil {
		return err
	}

	if flagVerbose {
		err = srcParagraph.WriteTo(os.Stdout)
		if err != nil {
			return err
		}
	}

	return nil
}

func main() {
	log.SetFlags(log.Lshortfile | log.LstdFlags)
	flag.Parse()

	if flagStatus {
		err := showStatus()
		if err != nil {
			log.Fatal(err)
		}
		return
	} else if flagRestore != "" {
		err := restore(flagRestore)
		if err != nil {
			log.Fatal(err)
		}
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

type debDetail struct {
	url       string
	jobDetail *jobDetail
}

type jobDetail struct {
	url      string
	prDetail *pullRequestDetail
}

type pullRequestDetail struct {
	pullRequestId
	url   string
	user  string
	title string
	state string
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

	err = installJobDebs(strings.TrimSuffix(targetUrl, "/console"), pr)
	return err
}

func installJobDebs(jobUrl string, pr *github.PullRequest) error {
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
			prDetail := new(pullRequestDetail)
			prDetail.num = pr.GetNumber()
			prDetail.repo = pr.GetBase().GetRepo().GetName()
			prDetail.url = pr.GetHTMLURL()
			prDetail.user = pr.GetUser().GetLogin()
			prDetail.title = pr.GetTitle()
			prDetail.state = pr.GetState()
			jobDetail := &jobDetail{
				url:      jobUrl,
				prDetail: prDetail,
			}
			err = installDeb(debUrl, pkgName, jobDetail)
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
	all, err := getAllPkgInstallDetails()
	if err != nil {
		return err
	}

	for _, detail := range all {
		fmt.Println("Repo:", detail["PR_REPO"])
		fmt.Println("Package:", detail["pkgs"])
		fmt.Println("Title:", detail["PR_TITLE"])
		fmt.Println("User:", detail["PR_USER"])
		fmt.Println("PR url:", detail["PR_URL"])
		fmt.Println("Job url:", detail["CI_URL"], "\n")
	}
	return nil
}

func getAllPkgInstallDetails() (allDetails map[string]map[string]string, err error) {
	fileInfos, err := ioutil.ReadDir(markDir)
	if err != nil {
		return
	}
	allDetails = make(map[string]map[string]string)
	for _, fileInfo := range fileInfos {
		pkg := fileInfo.Name()
		var detail map[string]string
		detail, err = getPkgInstallDetail(pkg)
		if err != nil {
			return
		}

		key := detail["CI_URL"]
		if key == "" {
			continue
		}
		v, ok := allDetails[key]
		if ok {
			v["pkgs"] = v["pkgs"] + " " + pkg
		} else {
			detail["pkgs"] = pkg
			allDetails[key] = detail
		}
	}
	return
}

func getPkgInstallDetail(pkg string) (detail map[string]string, err error) {
	out, err := sh.Command("dpkg-query", "-f", `${db:Status-Status}\n${Description}\n`, "--show", pkg).CombinedOutput()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if exitErr.ExitCode() == 1 {
				err = nil
				return
			}
		}
		return
	}

	lines := bytes.Split(out, []byte{'\n'})
	status := string(lines[0])
	if status != "installed" {
		return
	}

	var begin bool
	detail = make(map[string]string, 9)
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if !begin {
			if bytes.Equal(line, []byte("=begin")) {
				begin = true
			}
			continue
		} else {
			if bytes.Equal(line, []byte("=end")) {
				break
			}

			fields := bytes.SplitN(line, []byte{'='}, 2)
			if len(fields) != 2 {
				continue
			}

			key := string(fields[0])
			value := string(fields[1])
			detail[key] = value
		}
	}
	return
}

func restore(pattern string) error {
	all, err := getAllPkgInstallDetails()
	if err != nil {
		return err
	}

	var pkgs string
	for _, detail := range all {
		if pattern == "all" ||
			detail["PR_REPO"] == pattern ||
			detail["PR_USER"] == pattern {

			pkgs = pkgs + " " + detail["pkgs"]
		}
	}
	fmt.Println("restore", pkgs)

	// TODO mark uninstall

	err = sh.Command("sh", "-c", "sudo apt-get install "+pkgs).Run()
	return err
}
