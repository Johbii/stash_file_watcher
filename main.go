package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

type watcher []string

func (w *watcher) String() string {
	// how to print the watcher flag as a string for help and such
	return fmt.Sprint(*w)
}

func (w *watcher) Set(value string) error {
	f, err := os.Stat(value)
	if err != nil {
		return err
	}

	if !f.IsDir() {
		return errors.New("watcher path must be a directory")
	}

	for _, p := range *w {
		if strings.HasPrefix(filepath.Clean(value), filepath.Clean(p)) {
			if len(filepath.Clean(value)) == len(filepath.Clean(p)) || filepath.Clean(value)[len(filepath.Clean(p))] == filepath.Separator {
				return nil
			}
		}
	}

	abs, err := filepath.Abs(value)
	if err != nil {
		return err
	}

	*w = append(*w, abs)
	return nil
}

var watcherFlag watcher
var debugEnv bool
var verboseEnv bool
var doAuthEnv bool
var endpointEnv string
var scheduledScanIntervalMinsEnv string
var client *http.Client
var scanBody string

func init() {
	flag.Var(&watcherFlag, "watcher", "Path(s) to add watchers to (may be specified multiple times).")
	_, debugEnv = os.LookupEnv("STASH_WATCH_DEBUG")
	_, verboseEnv = os.LookupEnv("STASH_WATCH_VERBOSE")
	_, doAuthEnv = os.LookupEnv("STASH_WATCH_DO_AUTH")

	// scan options
	_, scanRescan := os.LookupEnv("STASH_WATCH_FORCE_RESCAN")
	_, scanGenerateClipPreviews := os.LookupEnv("STASH_WATCH_GEN_CLIP_PREV")
	_, scanGenerateCovers := os.LookupEnv("STASH_WATCH_GEN_COVER")
	_, scanGenerateImagePreviews := os.LookupEnv("STASH_WATCH_GEN_IMAGE_PREV")
	_, scanGeneratePhashes := os.LookupEnv("STASH_WATCH_GEN_PHASH")
	_, scanGeneratePreviews := os.LookupEnv("STASH_WATCH_GEN_PREV")
	_, scanGenerateSprites := os.LookupEnv("STASH_WATCH_GEN_SPRITE")
	_, scanGenerateThumbnails := os.LookupEnv("STASH_WATCH_GEN_THUMB")

	str := `{
"query": "mutation {
	metadataScan (input: {
		rescan: ` + fmt.Sprintf("%t", scanRescan) + `,
		scanGenerateClipPreviews: ` + fmt.Sprintf("%t", scanGenerateClipPreviews) + `,
		scanGenerateCovers: ` + fmt.Sprintf("%t", scanGenerateCovers) + `,
		scanGenerateImagePreviews: ` + fmt.Sprintf("%t", scanGenerateImagePreviews) + `,
		scanGeneratePhashes: ` + fmt.Sprintf("%t", scanGeneratePhashes) + `,
		scanGeneratePreviews: ` + fmt.Sprintf("%t", scanGeneratePreviews) + `,
		scanGenerateSprites: ` + fmt.Sprintf("%t", scanGenerateSprites) + `,
		scanGenerateThumbnails: ` + fmt.Sprintf("%t", scanGenerateThumbnails) + `
	})}"
}`
	printDebug("constructed scan request body:\n", str)

	str = regexp.MustCompile(`\n`).ReplaceAllString(str, "")
	scanBody = regexp.MustCompile(`\s+`).ReplaceAllString(str, " ")

	printDebug("final scan request body:", scanBody)

	var ok bool
	endpointEnv, ok = os.LookupEnv("STASH_API_ENDPOINT")
	if !ok {
		log.Fatal("Stash API endpoint is unset")
	}
	scheduledScanIntervalMinsEnv, ok = os.LookupEnv("STASH_SCAN_INTERVAL_MINS")
	if !ok {
		scheduledScanIntervalMinsEnv = "30"
	}

	tr := &http.Transport{
		MaxIdleConnsPerHost: 1024,
		TLSHandshakeTimeout: 0 * time.Second,
	}
	client = &http.Client{
		Transport: tr,
		Timeout:   3 * time.Second,
	}
}

func printVerbose(s ...any) {
	if !verboseEnv {
		return
	}

	args := make([]string, 0, len(s)+1)
	args = append(args, "VERBOSE:")
	for _, v := range s {
		args = append(args, fmt.Sprintf("%v", v))
	}

	log.Println(strings.Join(args, " "))
}

func printDebug(s ...any) {
	if !debugEnv {
		return
	}

	args := make([]string, 0, len(s)+1)
	args = append(args, "DEBUG:")
	for _, v := range s {
		args = append(args, fmt.Sprintf("%v", v))
	}

	log.Println(strings.Join(args, " "))
}

func printError(s ...any) {
	args := make([]string, 0, len(s)+1)
	args = append(args, "ERROR:")
	for _, v := range s {
		args = append(args, fmt.Sprintf("%v", v))
	}

	log.Println(strings.Join(args, " "))
}

func printInfo(s ...any) {
	args := make([]string, 0, len(s)+1)
	args = append(args, "INFO:")
	for _, v := range s {
		args = append(args, fmt.Sprintf("%v", v))
	}

	log.Println(strings.Join(args, " "))
}

func sendScanRequest(endpoint string, useAuthentication bool) {
	body := []byte(scanBody)
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		printError("could not create request", err)
		return
	}

	// does not support cookie-based authentication
	if useAuthentication {
		stashAPIKey, ok := os.LookupEnv("STASH_API_KEY")
		if !ok {
			printError(`authentication requested but API key is unset
(hint: make sure to include STASH_API_KEY environment variable)`)
			return
		}

		req.Header.Set("ApiKey", stashAPIKey)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		printError("error making http request", err)
		return
	}

	io.Copy(io.Discard, resp.Body) // <= NOTE must read response fully before closing for keep-alive
	defer resp.Body.Close()
}

func watchSubdirs(dir string, wat *fsnotify.Watcher) error {
	dirContents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	for _, p := range dirContents {
		if p.IsDir() {
			abs := filepath.Join(dir, p.Name())

			err = wat.Add(abs)
			if err != nil {
				log.Fatal(err)
			}
			printVerbose("Now watching", abs)
			// recurse
			watchSubdirs(abs, wat)
		}
	}

	return nil
}

func dedupLoop(wat *fsnotify.Watcher) {
	var (
		// wait 100ms for new events; each new event resets the timer
		waitFor = 100 * time.Millisecond

		// keep track of the timers, as path -> timer
		mu     sync.Mutex
		timers = make(map[string]*time.Timer)

		// callback
		processOp = func(event fsnotify.Event) {
			func() {
				// DO PROCESSING
				if event.Has(fsnotify.Create) {
					printVerbose("created path item:", event.Name)

					abs, err := filepath.Abs(event.Name)
					if err != nil {
						printError("error: could not get absolute path", err)
						return // SOFT ERROR
					}

					f, err := os.Stat(abs)
					if err != nil {
						printError("error: could not stat file", err)
						return // SOFT ERROR
					}

					if !f.IsDir() {
						printInfo("Files changed, sending update:", abs)
						// send update
						sendScanRequest(endpointEnv, doAuthEnv)
						return
					}

					err = wat.Add(abs)
					if err != nil {
						log.Fatal(err)
					}
					printInfo("New directory detected, now watching directory: ", abs)

					return
				}

				// can only reach if fs operation is write or remove

				printInfo("Files changed, sending update:", event.Name)
				// send update here
				sendScanRequest(endpointEnv, doAuthEnv)
			}()

			// destroying timer only necessary if you have many files
			mu.Lock()
			delete(timers, event.Name)
			mu.Unlock()
		}
	)
	for {
		select {
		case event, ok := <-wat.Events:
			if !ok { // channel was closed
				return
			}

			// we don't need to close watcher if dir is removed
			// inode is gone = watcher is gone

			// only listen on these operations
			if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Remove) && !event.Has(fsnotify.Create) {
				continue
			}

			printDebug("EVENT", event)

			mu.Lock()
			t, ok := timers[event.Name]
			mu.Unlock()
			if !ok {
				// run callback (debounced)
				t = time.AfterFunc(math.MaxInt64, func() { processOp(event) })
				t.Stop()

				mu.Lock()
				timers[event.Name] = t
				mu.Unlock()
			}

			t.Reset(waitFor)

		case err, ok := <-wat.Errors:
			if !ok { // channel was closed
				return // SOFT ERROR
			}
			printError(err)
		}
	}
}

func main() {
	flag.Parse()
	if len(flag.Args()) > 0 {
		log.Fatal("Unknown arguments: ", flag.Args())
	}
	if len(watcherFlag) < 1 {
		log.Fatal("Error: no watcher arguments provided. ",
			"Use '--watcher <PATH>' to start watching directories.")
	}

	for _, w := range watcherFlag {
		wat, err := fsnotify.NewWatcher()
		if err != nil {
			log.Fatal(err)
		}
		defer wat.Close()

		go dedupLoop(wat)

		err = wat.Add(w)
		if err != nil {
			log.Fatal(err)
		}
		printInfo("Initial watcher started at:", w)

		go watchSubdirs(w, wat)
	}

	d, err := strconv.Atoi(scheduledScanIntervalMinsEnv)
	if err != nil {
		log.Fatal("Error converting string to int:", err)
	}
	ticker := time.NewTicker(time.Duration(d) * time.Minute)
	defer ticker.Stop()

	go func() {
		for {
			select {
			case <-ticker.C:
				printInfo("Running scheduled scan")
				sendScanRequest(endpointEnv, doAuthEnv)
			}
		}
	}()

	sigInt := make(chan os.Signal, 1)
	signal.Notify(sigInt, os.Interrupt)

	for {
		select {
		case <-sigInt:
			printVerbose("Leaving")
			return
		}
	}
}
