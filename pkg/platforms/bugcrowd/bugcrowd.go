package bugcrowd

import (
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/PuerkitoBio/goquery"
	"github.com/hashicorp/go-retryablehttp"
    "github.com/savushkin-yauheni/bbscope/internal/utils"
	"github.com/savushkin-yauheni/bbscope/pkg/scope"
	"github.com/savushkin-yauheni/bbscope/pkg/whttp"
	"github.com/tidwall/gjson"
)

const (
	USER_AGENT               = "Mozilla/5.0 (X11; Linux x86_64; rv:82.0) Gecko/20100101 Firefox/82.0"
	RATE_LIMIT_SLEEP_SECONDS = 5
)

// Automated email + password login. 2FA needs to be disabled
func Login(email, password, proxy string) string {
	cookies := make(map[string]string)

	var loginChallenge string

	// Create a cookie jar
	jar, err := cookiejar.New(nil)
	if err != nil {
		utils.Log.Fatal(err)
	}

	// Create a retryablehttp client
	retryClient := retryablehttp.NewClient()

	retryClient.Logger = log.New(io.Discard, "", 0)

	retryClient.RetryMax = 5 // Set your retry policy

	// Set the standard client's cookie jar
	retryClient.HTTPClient.Jar = jar

	// Set proxy for custom client

	if proxy != "" {
		proxyURL, err := url.Parse(proxy)
		if err != nil {
			log.Fatal("Invalid Proxy String")
		}

		retryClient.HTTPClient.Transport = &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
				CipherSuites: []uint16{
					tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
					tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
				},
				PreferServerCipherSuites: true,
				MinVersion:               tls.VersionTLS11,
				MaxVersion:               tls.VersionTLS11},
		}
	}

	// Set the custom redirect policy on the underlying http.Client
	retryClient.HTTPClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		utils.Log.Debug("Redirecting to: ", req.URL)
		if strings.Contains(req.URL.String(), "login_challenge") {
			loginChallenge = strings.Split(req.URL.String(), "=")[1]
		}
		return nil // return nil to follow the redirect
	}

	firstRes, err := whttp.SendHTTPRequest(
		&whttp.WHTTPReq{
			Method: "GET",
			URL:    "https://identity.bugcrowd.com/login?user_hint=researcher&returnTo=/dashboard",
			Headers: []whttp.WHTTPHeader{
				{Name: "User-Agent", Value: USER_AGENT},
			},
		}, retryClient)

	if err != nil {
		utils.Log.Fatal(err)
	}

	if firstRes.StatusCode == 403 {
		utils.Log.Fatal("Got 403 on first request. You may be WAF banned. Change IP or wait")
	}

	var allCookiesString string
	for _, cookie := range firstRes.Headers["Set-Cookie"] {
		split := strings.Split(cookie, ";")
		cookies[split[0]] = split[1]
		allCookiesString += split[0] + "=" + split[1] + "; "
	}

	identityUrl, _ := url.Parse("https://identity.bugcrowd.com")
	csrfToken := ""
	for _, cookie := range retryClient.HTTPClient.Jar.Cookies(identityUrl) {
		if cookie.Name == "csrf-token" {
			csrfToken = cookie.Value
			break
		}
	}

	loginRes, err := whttp.SendHTTPRequest(
		&whttp.WHTTPReq{
			Method: "POST",
			URL:    "https://identity.bugcrowd.com/login",
			Headers: []whttp.WHTTPHeader{
				{Name: "User-Agent", Value: USER_AGENT},
				{Name: "X-Csrf-Token", Value: csrfToken},
				{Name: "Content-Type", Value: "application/x-www-form-urlencoded; charset=UTF-8"},
				{Name: "Origin", Value: "https://identity.bugcrowd.com"},
			},
			Body: "username=" + url.QueryEscape(email) + "&password=" + url.QueryEscape(password) + "&login_challenge=" + loginChallenge + "&otp_code=&backup_otp_code=&user_type=RESEARCHER&remember_me=true",
		}, retryClient)

	if err != nil {
		utils.Log.Fatal("Login request error: ", err)
	}

	if loginRes.StatusCode == 401 {
		utils.Log.Fatal("Login failed. Check your email and password. Make sure 2FA is off.")
	}

	_, err = whttp.SendHTTPRequest(
		&whttp.WHTTPReq{
			Method: "GET",
			URL:    gjson.Get(loginRes.BodyString, "redirect_to").String(),
			Headers: []whttp.WHTTPHeader{
				{Name: "User-Agent", Value: USER_AGENT},
				{Name: "Origin", Value: "https://identity.bugcrowd.com"},
			},
		}, retryClient)

	if err != nil {
		utils.Log.Fatal(err)
	}

	for _, cookie := range retryClient.HTTPClient.Jar.Cookies(identityUrl) {
		if cookie.Name == "_bugcrowd_session" {
			utils.Log.Info("Login OK. Fetching programs, please wait...")
			utils.Log.Debug("SESSION: ", cookie.Value)
			return cookie.Value
		}
	}

	utils.Log.Fatal("Unknown Error")
	return ""
}

func GetProgramHandles(sessionToken string, engagementType string, pvtOnly bool) ([]string, map[string]string) {
	pageIndex := 1

	listEndpointURL := "https://bugcrowd.com/engagements.json?category=" + engagementType + "&sort_by=promoted&sort_direction=desc&page="
	paths := []string{}
	dictionary := make(map[string]string)

	for {
		var res *whttp.WHTTPRes
		var err error

		res, err = whttp.SendHTTPRequest(
			&whttp.WHTTPReq{
				Method: "GET",
				URL:    listEndpointURL + strconv.Itoa(pageIndex),
				Headers: []whttp.WHTTPHeader{
					{Name: "Cookie", Value: "_bugcrowd_session=" + sessionToken},
					{Name: "User-Agent", Value: USER_AGENT},
				},
			}, nil)

		if err != nil {
			utils.Log.Fatal(err)
		}

		// Assuming res.BodyString is the JSON string response
		result := gjson.Get(string(res.BodyString), "engagements")

		// Bugcrowd's API sometimes tell us there are fewer pages than in reality, so we do it this way
		if len(result.Array()) == 0 {
			break
		}

		// Iterating over each element in the programs array
		result.ForEach(func(key, value gjson.Result) bool {
			programURL := value.Get("briefUrl").String()
			programName := value.Get("name").String()
			accessStatus := value.Get("accessStatus").String()

			if !pvtOnly || (pvtOnly && accessStatus != "open") {
				paths = append(paths, programURL)
				dictionary[programURL] = programName
			}

			// Return true to continue iterating
			return true
		})

		pageIndex++

	}

	return paths, dictionary
}

func GetProgramScope(handle string, categories string, token string, name string) (pData scope.ProgramData) {
	isEngagement := strings.HasPrefix(handle, "/engagements/")

	pData.Url = "https://bugcrowd.com" + handle
	pData.Name = name

	if isEngagement {
		getBriefVersionDocument := getEngagementBriefVersionDocument(handle, token)
		extractScopeFromEngagement(getBriefVersionDocument, token, &pData)
	} else {
		extractScopeFromTargetGroups(pData.Url, categories, token, &pData)
	}

	return pData
}

func getEngagementBriefVersionDocument(handle string, token string) string {
	res, err := whttp.SendHTTPRequest(
		&whttp.WHTTPReq{
			Method: "GET",
			URL:    "https://bugcrowd.com" + handle,
			Headers: []whttp.WHTTPHeader{
				{Name: "Cookie", Value: "_bugcrowd_session=" + token},
				{Name: "User-Agent", Value: USER_AGENT},
				{Name: "Accept", Value: "*/*"},
			},
		}, nil)

	if err != nil {
		utils.Log.Fatal(err)
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(res.BodyString))
	if err != nil {
		log.Fatal(err)
	}

	div := doc.Find("div[data-react-class='ResearcherEngagementBrief']")

	// Get the value of the data-api-endpoints attribute
	apiEndpointsJSON, exists := div.Attr("data-api-endpoints")
	if !exists {
		log.Fatal("data-api-endpoints attribute not found")
	}

	return gjson.Get(apiEndpointsJSON, "engagementBriefApi.getBriefVersionDocument").String() + ".json"
}

func extractScopeFromEngagement(getBriefVersionDocument string, token string, pData *scope.ProgramData) {
	res, err := whttp.SendHTTPRequest(
		&whttp.WHTTPReq{
			Method: "GET",
			URL:    "https://bugcrowd.com" + getBriefVersionDocument,
			Headers: []whttp.WHTTPHeader{
				{Name: "Cookie", Value: "_bugcrowd_session=" + token},
				{Name: "User-Agent", Value: USER_AGENT},
				{Name: "Accept", Value: "*/*"},
			},
		}, nil)

	if err != nil {
		utils.Log.Fatal(err)
	}

	// Extract the "scope" array from the JSON
	scopeArray := gjson.Get(res.BodyString, "data.scope")

	// Iterate over each element of the "scope" array
	scopeArray.ForEach(func(key, value gjson.Result) bool {
		// Check if the scope element is in-scope
		inScope := value.Get("inScope").Bool()

		// Extract the "targets" array for the current scope element
		targetsArray := value.Get("targets")

		// Iterate over each object in the "targets" array
		targetsArray.ForEach(func(objectKey, objectValue gjson.Result) bool {
			// Extract the "name", "uri", "category", and "description" fields for each object
			name := objectValue.Get("name").String()
			uri := objectValue.Get("uri").String()
			category := objectValue.Get("category").String()
			description := objectValue.Get("description").String()

			if uri == "" {
				uri = name
			}

			if inScope {
				pData.InScope = append(pData.InScope, scope.ScopeElement{Target: uri, Description: description, Category: category})
			} else {
				pData.OutOfScope = append(pData.OutOfScope, scope.ScopeElement{Target: uri, Description: description, Category: category})
			}

			return true
		})

		return true
	})
}
func extractScopeFromTargetGroups(url string, categories string, token string, pData *scope.ProgramData) {
	res, err := whttp.SendHTTPRequest(
		&whttp.WHTTPReq{
			Method: "GET",
			URL:    url + "/target_groups",
			Headers: []whttp.WHTTPHeader{
				{Name: "Cookie", Value: "_bugcrowd_session=" + token},
				{Name: "User-Agent", Value: USER_AGENT},
				{Name: "Accept", Value: "*/*"},
			},
		}, nil)

	if err != nil {
		utils.Log.Fatal(err)
	}

	for i, scopeTableURL := range gjson.Get(string(res.BodyString), "groups.#.targets_url").Array() {
		inScope := gjson.Get(string(res.BodyString), fmt.Sprintf("groups.%d.in_scope", i)).Bool()
		extractScopeFromTargetTable(scopeTableURL.String(), categories, token, pData, inScope)
	}
}
func extractScopeFromTargetTable(scopeTableURL string, categories string, token string, pData *scope.ProgramData, inScope bool) {
	res, err := whttp.SendHTTPRequest(
		&whttp.WHTTPReq{
			Method: "GET",
			URL:    "https://bugcrowd.com" + scopeTableURL,
			Headers: []whttp.WHTTPHeader{
				{Name: "Cookie", Value: "_bugcrowd_session=" + token},
				{Name: "User-Agent", Value: USER_AGENT},
				{Name: "Accept", Value: "*/*"},
			},
		}, nil)

	if err != nil {
		utils.Log.Fatal(err)
	}

	json := string(res.BodyString)
	targetsCount := gjson.Get(json, "targets.#").Int()

	for i := 0; i < int(targetsCount); i++ {
		targetPath := fmt.Sprintf("targets.%d", i)
		name := strings.TrimSpace(gjson.Get(json, targetPath+".name").String())
		uri := strings.TrimSpace(gjson.Get(json, targetPath+".uri").String())
		category := gjson.Get(json, targetPath+".category").String()
		description := gjson.Get(json, targetPath+".description").String()

		if categories != "all" && category != GetCategories(categories)[0] {
			continue
		}

		if uri == "" {
			uri = name
		}

		scopeElement := scope.ScopeElement{
			Target:      uri,
			Description: description,
			Category:    category,
		}

		if inScope {
			pData.InScope = append(pData.InScope, scopeElement)
		} else {
			pData.OutOfScope = append(pData.OutOfScope, scopeElement)
		}
	}
}

func GetCategories(input string) []string {
	categories := map[string][]string{
		"url":      {"website"},
		"api":      {"api"},
		"mobile":   {"android", "ios"},
		"android":  {"android"},
		"apple":    {"ios"},
		"other":    {"other"},
		"hardware": {"hardware"},
	}

	selectedCategory, ok := categories[strings.ToLower(input)]
	if !ok {
		utils.Log.Fatal("Invalid category")
	}
	return selectedCategory
}

func GetAllProgramsScope(token string, bbpOnly bool, pvtOnly bool, categories string, outputFlags string, concurrency int, delimiterCharacter string, includeOOS, printRealTime bool) (programs []scope.ProgramData) {
	programHandles, pathToName := GetProgramHandles(token, "bug_bounty", pvtOnly)

	utils.Log.Info("Fetching ", strconv.Itoa(len(programHandles)), " programs...")

	var mutex sync.Mutex
	handles := make(chan string, concurrency)
	processGroup := new(sync.WaitGroup)

	for i := 0; i < concurrency; i++ {
		processGroup.Add(1)
		go func() {
			defer processGroup.Done()
			for handle := range handles {
				pScope := GetProgramScope(handle, categories, token, pathToName[handle])

				mutex.Lock()
				programs = append(programs, pScope)
				mutex.Unlock()
			}
		}()
	}

	for _, handle := range programHandles {
		handles <- handle
	}

	close(handles)
	processGroup.Wait()
	return programs
}

// PrintAllScope prints to stdout all scope elements of all targets
func PrintAllScope(token string, bbpOnly bool, pvtOnly bool, categories string, outputFlags string, concurrency int, delimiter string, includeOOS, printRealTime bool) {
	programs := GetAllProgramsScope(token, bbpOnly, pvtOnly, categories, outputFlags, concurrency, delimiter, includeOOS, printRealTime)
	scope.PrintProgramScope(programs, outputFlags, delimiter)
}

/*
// ListPrograms prints a list of available programs
func ListPrograms(token string, bbpOnly bool, pvtOnly bool) {
	programPaths := GetProgramPagePaths(token, bbpOnly, pvtOnly)
	for _, path := range programPaths {
		fmt.Println("https://bugcrowd.com" + path)
	}
}*/
