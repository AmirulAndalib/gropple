package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"strconv"

	"github.com/gorilla/mux"
)

type download struct {
	Id       int      `json:"id"`
	Url      string   `json:"url"`
	Pid      int      `json:"pid"`
	ExitCode int      `json:"exit_code"`
	State    string   `json:"state"`
	Finished bool     `json:"finished"`
	Files    []string `json:"files"`
	Eta      string   `json:"eta"`
	Percent  float32  `json:"percent"`
	Log      []string `json:"log"`
}

var downloads []*download
var downloadId = 0
var downloadPath = "./"

//go:embed web
var webFS embed.FS

func main() {
	var address string
	flag.StringVar(&address, "address", "", "address for the service")
	flag.StringVar(&downloadPath, "path", "", "path for downloaded files - defaults to current directory")
	flag.Parse()

	r := mux.NewRouter()
	r.HandleFunc("/", HomeHandler)
	r.HandleFunc("/fetch", FetchHandler)
	r.HandleFunc("/fetch/info/{id}", FetchInfoHandler)

	http.Handle("/", r)

	srv := &http.Server{
		Handler: r,
		Addr:    "127.0.0.1:8000",
		// Good practice: enforce timeouts for servers you create!
		WriteTimeout: 0 * time.Second,
		ReadTimeout:  0 * time.Second,
	}

	go func() {
		for {
			//fmt.Printf("\n\n%#V\n\n", downloads)
			time.Sleep(time.Second)
		}
	}()

	log.Fatal(srv.ListenAndServe())

}

func HomeHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)

	bookmarkletURL := "javascript:(function(f,s,n,o){window.open(f+encodeURIComponent(s),n,o)}('http://localhost:8000/fetch?url=',window.location,'yourform','width=500,height=500'));"

	t, err := template.ParseFS(webFS, "web/layout.tmpl", "web/index.html")
	if err != nil {
		panic(err)
	}

	type Info struct {
		Downloads      []*download
		BookmarkletURL template.URL
	}

	info := Info{
		Downloads:      downloads,
		BookmarkletURL: template.URL(bookmarkletURL),
	}

	log.Printf("%s", info.BookmarkletURL)
	err = t.ExecuteTemplate(w, "layout", info)
	if err != nil {
		panic(err)
	}

}

func FetchInfoHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	idString := vars["id"]
	if idString != "" {
		id, err := strconv.Atoi(idString)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		for _, dl := range downloads {
			if dl.Id == id {
				b, _ := json.Marshal(dl)
				w.Write(b)
				return
			}
		}
	} else {
		http.NotFound(w, r)
	}
}

func FetchHandler(w http.ResponseWriter, r *http.Request) {

	query := r.URL.Query()
	url, present := query["url"] //filters=["color", "price", "brand"]

	if !present {
		fmt.Fprint(w, "something")
	} else {

		// create the record
		// XXX should be atomic!
		downloadId++
		newDownload := download{
			Id:       downloadId,
			Url:      url[0],
			State:    "starting",
			Finished: false,
			Eta:      "?",
			Percent:  0.0,
			Log:      make([]string, 0, 1000),
		}
		downloads = append(downloads, &newDownload)
		// XXX atomic ^^

		newDownload.Log = append(newDownload.Log, "start of log...")

		go func() {
			queue(&newDownload)
		}()
		t, err := template.ParseFS(webFS, "web/layout.tmpl", "web/popup.html")
		if err != nil {
			panic(err)
		}
		err = t.ExecuteTemplate(w, "layout", newDownload)
		if err != nil {
			panic(err)
		}

		//		fmt.Fprintf(w, "Started DL %d!", downloadId)
	}
}

func queue(dl *download) {

	cmd := exec.Command(
		"youtube-dl",
		"--write-info-json",
		"-f", "bestvideo[ext=mp4]+bestaudio[ext=m4a]/best[ext=mp4]/best",
		"--newline", dl.Url,
	)
	cmd.Dir = downloadPath

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		dl.State = "failed"
		dl.Finished = true
		dl.Log = append(dl.Log, fmt.Sprintf("error setting up stdout pipe: %v", err))
		return
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		dl.State = "failed"
		dl.Finished = true
		dl.Log = append(dl.Log, fmt.Sprintf("error setting up stderr pipe: %v", err))
		return
	}

	err = cmd.Start()
	if err != nil {
		dl.State = "failed"
		dl.Finished = true
		dl.Log = append(dl.Log, fmt.Sprintf("error starting youtube-dl: %v", err))
		return
	}
	dl.Pid = cmd.Process.Pid

	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		updateDownload(stdout, dl)
	}()

	go func() {
		defer wg.Done()
		updateDownload(stderr, dl)
	}()

	wg.Wait()
	cmd.Wait()

	dl.State = "complete"
	dl.Finished = true
	dl.ExitCode = cmd.ProcessState.ExitCode()

	fmt.Printf("OBJ %#v\n", dl)
}

func updateDownload(r io.Reader, dl *download) {
	// XXX not sure if we might get a partial line?
	buf := make([]byte, 1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			s := string(buf[:n])
			lines := strings.Split(s, "\n")

			for _, l := range lines {

				if l == "" {
					continue
				}

				// append the raw log
				dl.Log = append(dl.Log, l)

				// look for the percent and eta and other metadata
				updateMetadata(dl, l)
			}
		}
		if err != nil {
			break
		}
	}
}

func updateMetadata(dl *download, s string) {

	// [download]  49.7% of ~15.72MiB at  5.83MiB/s ETA 00:07
	etaRE := regexp.MustCompile(`download.+ETA +(\d\d:\d\d)`)
	matches := etaRE.FindStringSubmatch(s)
	if len(matches) == 2 {
		dl.Eta = matches[1]
		dl.State = "downloading"

	}

	percentRE := regexp.MustCompile(`download.+?([\d\.]+)%`)
	matches = percentRE.FindStringSubmatch(s)
	if len(matches) == 2 {
		p, err := strconv.ParseFloat(matches[1], 32)
		if err == nil {
			dl.Percent = float32(p)
		} else {
			panic(err)
		}
	}

	// This appears once per destination file
	// [download] Destination: Filename with spaces and other punctuation here be careful!.mp4
	filename := regexp.MustCompile(`download.+?Destination: (.+)$`)
	matches = filename.FindStringSubmatch(s)
	if len(matches) == 2 {
		dl.Files = append(dl.Files, matches[1])
	}
}
