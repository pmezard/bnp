package main

import (
	"bytes"
	"compress/flate"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"regexp"

	"github.com/pmezard/pdf"
)

// MultiCloser references a sequence of io.ReadCloser, delegates writes to the
// last one and close all of them in order in Close(). Use it when stacking
// filters one onto another.
type MultiCloser struct {
	Readers []io.ReadCloser
}

func (r *MultiCloser) Read(data []byte) (int, error) {
	return r.Readers[len(r.Readers)-1].Read(data)
}

func (r *MultiCloser) Close() error {
	var err error
	for _, r := range r.Readers {
		e := r.Close()
		if e != nil {
			err = e
		}
	}
	return err
}

// extractStream takes a raw PDF object stream and the list of its filters and
// returns an io.Reader applying all filters on it.
func extractStream(r io.ReadCloser, filters []string) (io.ReadCloser, error) {
	readers := []io.ReadCloser{r}
	for _, f := range filters {
		if f == "FlateDecode" {
			r = flate.NewReader(r)
			readers = append(readers, r)
		} else {
			return nil, fmt.Errorf("unknown stream filter: %s", f)
		}
	}
	return &MultiCloser{
		Readers: readers,
	}, nil
}

// walk traverses a pdf.Value graph while avoiding cycles by tracking object
// pointers. callback is invoked for each visited value, in pre-order. If the
// callback returns an error, the traversal stops and the error is forwarded to
// the caller.
func walk(root pdf.Value, callback func(v pdf.Value) error) error {
	seen := map[uint32]struct{}{}
	var walkNode func(pdf.Value, int) error
	walkNode = func(v pdf.Value, depth int) error {
		err := callback(v)
		if err != nil {
			return err
		}
		switch v.Kind() {
		case pdf.Dict:
			for _, k := range v.Keys() {
				id := v.KeyId(k)
				if _, ok := seen[id]; ok {
					continue
				}
				seen[id] = struct{}{}
				err := walkNode(v.Key(k), depth+1)
				delete(seen, id)
				if err != nil {
					return err
				}
			}
		case pdf.Array:
			l := v.Len()
			for i := 0; i < l; i++ {
				id := v.IndexId(i)
				if _, ok := seen[id]; ok {
					continue
				}
				seen[id] = struct{}{}
				err := walkNode(v.Index(i), depth+1)
				delete(seen, id)
				if err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walkNode(root, 0)
}

// tokenize tokenizes a PDF actions stream ad invoke callback with the name and
// the arguments of each extracted actions. The arguments are possible values
// returned by pdf.Tokenize.
func tokenize(r io.Reader, callback func(keyword string, args []interface{}) error) error {
	data, err := ioutil.ReadAll(r)
	if err != nil {
		return err
	}
	tokens, err := pdf.Tokenize(bytes.NewBuffer(data))
	if err != nil {
		ioutil.WriteFile("dump", data, 0644)
		return fmt.Errorf("could not tokenize: %s", err)
	}
	args := []interface{}{}
	for _, t := range tokens {
		if t.Kind == "keyword" {
			err := callback(t.Value.(string), args)
			if err != nil {
				return err
			}
			args = args[:0]
		} else {
			args = append(args, t.Value)
		}
	}
	return nil
}

// Word is a single text command in a PDF stream, annotated with its column
// location in the document.
type Word struct {
	Column float64
	S      string
}

type sortedWords []Word

func (s sortedWords) Len() int {
	return len(s)
}
func (s sortedWords) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}
func (s sortedWords) Less(i, j int) bool {
	return s[i].Column < s[j].Column
}

func f64(v interface{}) float64 {
	i, ok := v.(int64)
	if ok {
		return float64(i)
	}
	return v.(float64)
}

type Line struct {
	Value string
	Words []Word
}

// extractStreamLines parses a PDF action stream, extract text bits and attemps
// to group them by line using the text matrices offsets. It returns a sequence
// of lines from top to bottom.
func extractStreamLines(r io.Reader) ([]Line, error) {
	lines := map[float64][]Word{}
	x, y := 0., 0.
	text := false
	err := tokenize(r, func(keyword string, args []interface{}) error {
		switch keyword {
		case "BT": // Begin text object
			text = true
		case "ET": // End text object
			text = false
		case "Tj": // Show text
			lines[y] = append(lines[y], Word{
				Column: x,
				S:      args[0].(string),
			})
		case "Tm": // set text matrix
			x = f64(args[4])
			y = f64(args[5])
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	ys := []float64{}
	for y, words := range lines {
		sort.Sort(sortedWords(words))
		ys = append(ys, y)
	}
	sort.Float64s(ys)
	for i := 0; i < len(ys)/2; i++ {
		j := len(ys) - i - 1
		ys[i], ys[j] = ys[j], ys[i]
	}
	result := []Line{}
	for _, y := range ys {
		parts := []string{}
		for _, w := range lines[y] {
			parts = append(parts, w.S)
		}
		result = append(result, Line{
			Value: strings.Join(parts, " "),
			Words: lines[y],
		})
	}
	return result, err
}

// Op represent a line in the bank report. They come in two kinds: account
// state if IsTotal is true, account change otherwise. Value is expressed in
// eurocents. Date is unstructured and depends on the type of record. Source is
// the entry lable and SourceCol its column location in the PDF page.
type Op struct {
	Date      string
	Source    string
	SourceCol float64
	Value     int64
	HasValue  bool
	IsTotal   bool
}

var (
	reDigits = regexp.MustCompile(`^\d+$`)
)

// stripValue takes a []Word, attemps to extract a trailing amount like
// "123,45" or "12.345,67" and returns the stripped words and success.
func stripValue(line string, words []Word) ([]Word, int64, bool) {
	if len(words) < 3 {
		return words, 0, false
	}
	lw := len(words)
	head := words[lw-3].S
	dot := words[lw-2].S
	tail := words[lw-1].S
	// 123,45
	if reDigits.MatchString(head) && dot == "," && reDigits.MatchString(tail) &&
		len(tail) == 2 {
		n := 3
		num := head + tail
		// 3.123.45
		if lw > 4 && words[lw-4].S == "." && reDigits.MatchString(words[lw-5].S) {
			num = words[lw-5].S + num
			n = 5
		}
		v, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			return words, 0, false
		}
		if words[lw-n].Column < 500 {
			v = -v
		}
		return words[:lw-n], v, true
	}
	return words, 0, false
}

// stripDate attemps to extract a leading date like "13.06" and returns the
// stripped words on success.
func stripDate(line string, words []Word) ([]Word, string) {
	lw := len(words)
	if lw < 3 {
		return words, ""
	}
	head := words[0].S
	dot := words[1].S
	tail := words[2].S
	if !reDigits.MatchString(head) || dot != "." || !reDigits.MatchString(tail) {
		return words, ""
	}
	return words[3:], words[0].S + words[1].S + words[2].S
}

func joinWords(words []Word) string {
	parts := []string{}
	for _, w := range words {
		parts = append(parts, w.S)
	}
	return strings.Join(parts, " ")
}

var (
	reStart = regexp.MustCompile(`^SOLDE\s+.*(\d{2}\.\d{2}\.\d{4})`)
)

// parseTotalLine attempts to parse an account state line. It returns a nil Op
// if the line does not look like it, or an error.
func parseTotalLine(line Line) (*Op, error) {
	m := reStart.FindStringSubmatch(line.Value)
	if m == nil {
		return nil, nil
	}
	w, v, ok := stripValue(line.Value, line.Words)
	if !ok {
		return nil, fmt.Errorf("could not parse total line: %s", line.Value)
	}
	return &Op{
		Source:    joinWords(w),
		SourceCol: -1,
		Date:      m[1],
		Value:     v,
		HasValue:  true,
		IsTotal:   true,
	}, nil
}

// parseOpLine attempts to parse an account change line. This is made
// complicated by the fact an account change can be made of multiple lines
// carrying various information like:
//
//  26.02 SOURCE
//        SOURCE CONTINUED
//        SOURCE CONTINUED  123.34
//
// Returned Op can be partial, that is have only a date and source, only a
// source or only a source and value.
func parseOpLine(line Line) (*Op, error) {
	op := &Op{}
	words := line.Words
	w, date := stripDate(line.Value, words)
	words = w
	if date != "" {
		op.Date = date
	}
	w, v, offset := stripValue(line.Value, words)
	words = w
	if offset {
		op.Value = v
		op.HasValue = true
	}
	w, v, offset = stripValue(line.Value, words)
	if offset {
		// Invalid summary "TOTAL DES MONTANTS" line
		return nil, nil
	}
	if len(words) > 0 {
		op.SourceCol = words[0].Column
	}
	op.Source += joinWords(words)
	return op, nil
}

// parseOps returns a sequence of Ops extracted from a single stream. Partial
// operations are consolidated.
func parseOps(lines []Line) ([]*Op, error) {
	ops := []*Op{}
	for _, line := range lines {
		if strings.HasPrefix(line.Value, "TOTAL DES MONTANTS") {
			continue
		}
		if strings.HasPrefix(line.Value, "BNP PARIBAS SA : capital de") ||
			strings.HasPrefix(line.Value, "Montant de votre autorisation") {
			break
		}
		op, err := parseTotalLine(line)
		if err != nil {
			return nil, err
		}
		if op == nil {
			op, err = parseOpLine(line)
			if err != nil {
				return nil, err
			}
		}
		if op == nil {
			continue
		}
		var prev *Op
		if len(ops) > 0 {
			prev = ops[len(ops)-1]
		}

		if op.Date != "" {
			// Append
			ops = append(ops, op)
		} else {
			if prev != nil && (op.HasValue || op.Source != "") {
				// Merge
				if op.HasValue {
					prev.Value = op.Value
					prev.HasValue = true
				}
				if op.Source != "" && op.SourceCol == prev.SourceCol {
					prev.Source += op.Source
				}
			}
		}
	}
	return ops, nil
}

// filterOnSourceColumn assumes the Ops are either account states or changes,
// and that changes are always formatted like described in parseOpLine. Using
// the most popular SourceCol it then weeds out lines looking like changes
// which are not.
func filterOnSourceColumn(ops []*Op) []*Op {
	cols := map[float64]int{}
	maxCol := float64(-1)
	maxCount := -1
	for _, op := range ops {
		if op.SourceCol >= 0 {
			n := cols[op.SourceCol] + 1
			if n > maxCount {
				maxCount = n
				maxCol = op.SourceCol
			}
			cols[op.SourceCol] = n
		}
	}
	kept := []*Op{}
	for _, op := range ops {
		if op.SourceCol < 0 || op.SourceCol == maxCol {
			kept = append(kept, op)
		}
	}
	return kept
}

// extractOps returns all operations from a single page pdf.Value, filtered.
func extractOps(v pdf.Value) ([]*Op, error) {
	allOps := []*Op{}
	err := walk(v, func(v pdf.Value) error {
		if v.Kind() != pdf.Stream {
			return nil
		}
		filters := []string{}
		for _, k := range v.Keys() {
			// Only for Type1/TrueType fonts
			if k == "Length1" ||
				k == "Subtype" && v.Key(k).Name() == "Image" {
				return nil
			}
			if k != "Filters" {
				continue
			}
			values := v.Key(k)
			l := values.Len()
			for i := 0; i < l; i++ {
				filters = append(filters, values.Index(i).Name())
			}
		}
		r, err := extractStream(v.Reader(), filters)
		if err != nil {
			return err
		}
		lines, err := extractStreamLines(r)
		r.Close()
		if err != nil {
			headers := &bytes.Buffer{}
			for _, k := range v.Keys() {
				fmt.Fprintf(headers, "%s: %s\n", k, v.Key(k))
			}
			return fmt.Errorf("could not parse stream: %s\n%s\n", err, headers.String())
		}
		ops, err := parseOps(lines)
		if err != nil {
			return err
		}
		allOps = append(allOps, ops...)
		return nil
	})
	return filterOnSourceColumn(allOps), err
}

func hashOp(op *Op) string {
	return op.Date + "-" + op.Source + "-" + fmt.Sprintf("%f", op.Value)
}

// extractPDFOps returns all operations in a PDF report, deduplicated.
func extractPDFOps(r *pdf.Reader) ([]*Op, error) {
	seen := map[string]bool{}
	pages := r.NumPage()
	allOps := []*Op{}
	for i := 0; i < pages; i++ {
		ops, err := extractOps(r.Page(i + 1).V)
		if err != nil {
			return nil, err
		}
		for _, op := range ops {
			h := hashOp(op)
			if seen[h] {
				continue
			}
			seen[h] = true
			allOps = append(allOps, op)
		}
	}
	return allOps, nil
}

// Value is the state of an account at a given date, after applying the
// operation described by Source. Value is in eurocents.
type Value struct {
	Date   time.Time
	Source string
	Value  int64
}

const (
	dateFormat = "02.01.2006"
)

// convertOptsToValues takes all operations of a report, check they start and
// end with an account state entry, applies changes iteratively and check the
// intermediate states match parsed states. Corresponding Values are returned.
func convertOpsToValues(ops []*Op) ([]Value, error) {
	if len(ops) < 2 {
		return nil, fmt.Errorf("not enough operations in report: %d", len(ops))
	}
	first := ops[0]
	if !first.IsTotal {
		return nil, fmt.Errorf("first operation is not an account record: %+v", *first)
	}
	last := ops[len(ops)-1]
	if !last.IsTotal {
		return nil, fmt.Errorf("last operation is not an account record: %+v", *last)
	}
	values := []Value{}
	total := first.Value
	for _, op := range ops {
		var date time.Time
		var err error
		if op.IsTotal {
			if op.Value != total {
				return nil, fmt.Errorf(
					"running total does not match account record %+v: %d != %d",
					op, op.Value, total)
			}
			date, err = time.Parse(dateFormat, op.Date)
			if err != nil {
				return nil, err
			}
		} else {
			total += op.Value
			if len(values) == 0 {
				return nil, fmt.Errorf("operation without an account record: %+v", op)
			}
			prevDate := values[len(values)-1].Date
			date, err = time.Parse(dateFormat,
				fmt.Sprintf("%s.%d", op.Date, prevDate.Year()))
			if err != nil {
				return nil, err
			}
			if prevDate.After(date) {
				// Year transition
				date, err = time.Parse(dateFormat,
					fmt.Sprintf("%s.%d", op.Date, prevDate.Year()+1))
				if err != nil {
					return nil, err
				}
			}
		}
		values = append(values, Value{
			Date:   date,
			Source: op.Source,
			Value:  total,
		})
	}
	return values, nil
}

func extractFileValues(files []string) ([]Value, error) {
	failed := 0
	fail := func(fn string, err error) {
		fmt.Fprintf(os.Stderr, "error: %s: %s\n", fn, err)
		failed += 1
	}
	allValues := []Value{}
	for _, file := range files {
		r, err := pdf.Open(file)
		if err != nil {
			return nil, err
		}
		ops, err := extractPDFOps(r)
		if err != nil {
			fail(file, err)
			continue
		}
		values, err := convertOpsToValues(ops)
		if err != nil {
			fail(file, err)
			continue
		}
		prev := int64(-1)
		for _, v := range values {
			d := v.Date.Format("2006-01-02")
			h := v.Value / 100
			l := v.Value % 100
			dh := 0
			dl := 0
			if prev >= 0 {
				delta := int(v.Value - prev)
				if delta > 0 {
					dh = delta / 100
					dl = delta % 100
				} else {
					dh = -((-delta) / 100)
					dl = (-delta) % 100
				}
			}
			prev = v.Value
			fmt.Printf("%s - %6d.%02d / %4d.%02d - %s\n", d, h, l, dh, dl, v.Source)
		}
		allValues = append(allValues, values...)
	}
	if failed > 0 {
		return nil, fmt.Errorf("%d reports failed\n", failed)
	}
	return allValues, nil
}

func writeJsonValues(values []Value, path string) error {
	fp, err := os.Create(path)
	if err != nil {
		return err
	}
	err = json.NewEncoder(fp).Encode(values)
	err2 := fp.Close()
	if err != nil {
		return err
	}
	return err2
}

var (
	parseCmd   = app.Command("parse", "parse BNP Paribas PDF reports")
	parseFiles = parseCmd.Arg("files", "PDF files to parse").Strings()
	parseJson  = parseCmd.Flag("json", "path to JSON output file").String()
)

func parseFn() error {
	if len(*parseFiles) < 1 {
		return fmt.Errorf("no PDF file specified")
	}
	values, err := extractFileValues(*parseFiles)
	if err != nil {
		return err
	}
	if *parseJson != "" {
		err = writeJsonValues(values, *parseJson)
		if err != nil {
			return err
		}
	}
	return nil
}
