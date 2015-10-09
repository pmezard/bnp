package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
)

type WebValue struct {
	X      int64  `json:"x"`
	Y      int64  `json:"y"`
	Source string `json:"n"`
	Delta  int64  `json:"d"`
}

func readJsonValues(path string) ([]Value, error) {
	values := []Value{}
	fp, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	err = json.NewDecoder(fp).Decode(&values)
	return values, err
}

// embedJson replaces the $DATA$ placeholder in html with the javascript
// representation of input values. It embeds values as json data.
func embedJson(html []byte, values []Value) ([]byte, error) {
	webs := make([]WebValue, 0, len(values))
	for i, v := range values {
		delta := int64(0)
		if i > 0 {
			delta = v.Value - values[i-1].Value
		}
		webs = append(webs, WebValue{
			X:      v.Date.Unix(),
			Y:      v.Value,
			Source: v.Source,
			Delta:  delta,
		})
	}
	buf := &bytes.Buffer{}
	err := json.NewEncoder(buf).Encode(&webs)
	if err != nil {
		return nil, err
	}
	data := bytes.Replace(html, []byte("$DATA$"), buf.Bytes(), 1)
	return data, nil
}

type Matcher func(string) bool

// parseIgnoreRules returns a Value.Source matcher from input lines. Empty
// lines or lines starting with # are ignored. Others are pieced together as
// alternatives of a single regular expression. Returned matcher succeeds if
// one of the alternative matches the input string.
func parseIgnoreRules(r io.Reader) (Matcher, error) {
	rules := []string{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rules = append(rules, line)
	}
	if scanner.Err() != nil {
		return nil, scanner.Err()
	}
	if len(rules) == 0 {
		return func(s string) bool {
			return false
		}, nil
	}
	expr := "^.*(?:" + strings.Join(rules, "|") + ").*$"
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, err
	}
	return func(s string) bool {
		return re.MatchString(s)
	}, nil
}

func readIgnoreFile(path string) (Matcher, error) {
	fp, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fp.Close()
	return parseIgnoreRules(fp)
}

// filterValues removes matched values from the input sequence, and adjusts the
// following values as if the removed operations had never existed.
func filterValues(values []Value, m Matcher) []Value {
	if len(values) == 0 {
		return values
	}
	kept := []Value{}
	for i, v := range values {
		if m(v.Source) {
			continue
		}
		if len(kept) > 0 {
			// Apply the account change relatively to kept values
			delta := v.Value - values[i-1].Value
			v.Value = kept[len(kept)-1].Value + delta
		}
		kept = append(kept, v)
	}
	return kept
}

var (
	webCmd = app.Command("web", `run charts web frontend

web takes a sequence of JSON values and plots them in HTML at specified address.

An ignore files can be supplied to remove values from the sequence and make it
like they never existed. The ignore file lines are regular expression partially
matching the source of values to remove. Empty line or lines starting with #
are ignored.

`)
	webValues = webCmd.Arg("values", "JSON values to display").Required().String()
	webAddr   = webCmd.Flag("http", "web server address").
			Default("localhost:8081").String()
	webIgnorePath = webCmd.Flag("ignore", "path to ignore file").String()
)

func webFn() error {
	values, err := readJsonValues(*webValues)
	if err != nil {
		return err
	}
	http.Handle("/scripts/", http.FileServer(http.Dir(".")))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		kept := values
		if *webIgnorePath != "" {
			ignore, err := readIgnoreFile(*webIgnorePath)
			if err != nil {
				log.Println(err)
				return
			}
			kept = filterValues(kept, ignore)
		}
		if len(kept) == 0 {
			log.Println("all values were filtered")
			return
		}
		html, err := ioutil.ReadFile("scripts/main.html")
		if err != nil {
			log.Println(err)
			return
		}
		html, err = embedJson(html, kept)
		if err != nil {
			log.Println(err)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write(html)
	})
	return http.ListenAndServe(*webAddr, nil)
}
