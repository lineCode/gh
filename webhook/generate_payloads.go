// +build ignore

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"tmpl"
	"unicode"

	"github.com/PuerkitoBio/goquery"
)

const docURL = "https://developer.github.com/v3/activity/events/types"

var (
	output   string
	testdata string
)

type rawEvent struct {
	Name        string
	PayloadJSON string
}

type member struct {
	Name string
	Typ  string
	Tag  string
}

type object struct {
	Name    string
	Members []member
}

type memberSet []member

func (ms memberSet) Search(name string) int {
	return sort.Search(len(ms), func(i int) bool { return ms[i].Name >= name })
}

func (ms *memberSet) Add(m member) {
	// Member's type can be empty, when it duplicates type of its parent;
	// ignore it as recursive types are not supported.
	if m.Typ == "" {
		return
	}
	switch i := ms.Search(m.Name); {
	case i == len(*ms):
		*ms = append(*ms, m)
	case (*ms)[i].Name == m.Name:
		if typ := (*ms)[i].Typ; typ != m.Typ {
			// The "created_at" and "pushed_at" keys storing timestamps instead
			// of RFC3339 time for PushEvent looks like a bug. Force use of
			// time.Time type.
			if m.Name == "CreatedAt" || m.Name == "PushedAt" {
				(*ms)[i].Typ = "time.Time"
				break
			}
			(*ms)[i].Typ = "interface{}"
			fmt.Fprintf(os.Stderr, "different types for %s member: %s and %s, using interface{}\n", m.Name, typ, m.Typ)
		}
	default:
		*ms = append(*ms, member{})
		copy((*ms)[i+1:], (*ms)[i:])
		(*ms)[i] = m
	}
}

type objectSet []object

func (os objectSet) Search(name string) int {
	return sort.Search(len(os), func(i int) bool { return os[i].Name >= name })
}

func (os *objectSet) Add(o object) {
	switch i := os.Search(o.Name); {
	case i == len(*os):
		*os = append(*os, o)
	case (*os)[i].Name == o.Name:
		for _, m := range o.Members {
			// Ignore members which are named after structs to not create
			// invalid recursive types.
			if o.Name == m.Name && m.Name == m.Typ {
				continue
			}
			(*memberSet)(&(*os)[i].Members).Add(m)
		}
	default:
		*os = append(*os, object{})
		copy((*os)[i+1:], (*os)[i:])
		(*os)[i] = o
	}
}

const header = `// Created by go generate; DO NOT EDIT

package webhook

import (
	"reflect"
	"time"
)

var payloadTypes = map[string]reflect.Type{
{{range $_, $event := .}}	"{{snakeCase $event.Name}}": reflect.TypeOf((*{{$event.Name}})(nil)).Elem(),
{{end}}
}
`

const types = `{{range $_, $o := .}}// {{$o.Name}} was autogenerated by go generate. To see more details about this
// payload type visit https://developer.github.com/v3/activity/events/types.
type {{$o.Name}} struct {
{{range $_, $m := $o.Members}}	{{$m.Name}} {{$m.Typ}} ` + "`json:\"{{$m.Tag}}\"`" + `
{{end}}
}
{{end}}
// Files was autogenerated by go generate. To see more details about this
// payload type visit https://developer.github.com/v3/activity/events/types.
type Files map[string]File
`

var tmplHeader = template.Must(template.New("payloads").Funcs(map[string]interface{}{"snakeCase": snakeCase}).Parse(header))
var tmplTypes = template.Must(template.New("payloads").Parse(types))

// Those keys that are assigned to null in example JSON payloads lack type
// information. Instead the value types are mapped here by hand.
var hardcodedTypes = map[string]string{
	"user":        "User",
	"position":    "int",
	"line":        "int",
	"closed_at":   "time.Time",
	"merged_at":   "time.Time",
	"body":        "string",
	"path":        "string",
	"homepage":    "string",
	"language":    "string",
	"mirror_url":  "string",
	"assignee":    "string",
	"milestone":   "string",
	"message":     "string",
	"merged_by":   "string",
	"base_ref":    "string",
	"summary":     "string",
	"name":        "string",
	"target_url":  "string",
	"description": "string",
}

// File is a value of gist's Files map, it's handled separately as
// linearObjects does not handle type aliasing.
//
// https://developer.github.com/v3/gists/
var hardcodedFileType = object{
	Name: "File",
	Members: []member{
		{
			Name: "Size",
			Typ:  "int",
			Tag:  "size",
		},
		{
			Name: "RawURL",
			Typ:  "string",
			Tag:  "raw_url",
		},
		{
			Name: "Type",
			Typ:  "string",
			Tag:  "type",
		},
		{
			Name: "Truncated",
			Typ:  "bool",
			Tag:  "truncated",
		},
		{
			Name: "Language",
			Typ:  "string",
			Tag:  "language",
		},
	},
}

var idiomaticReplacer = strings.NewReplacer("Url", "URL", "Id", "ID", "Html", "HTML", "Sha", "SHA")

func nonil(err ...error) error {
	for _, err := range err {
		if err != nil {
			return err
		}
	}
	return nil
}

func die(v interface{}) {
	fmt.Fprintln(os.Stderr, v)
	os.Exit(1)
}

func init() {
	flag.StringVar(&output, "o", "payloads.go", "Output generated Go structs to this file.")
	dump := flag.Bool("t", false, "Write all intermediate JSON files for testing.")
	flag.Parse()
	if !filepath.IsAbs(output) {
		s, err := filepath.Abs(output)
		if err != nil {
			die(err)
		}
		output = s
	}
	if *dump {
		testdata = filepath.Join(filepath.Dir(output), "testdata")
		if err := os.MkdirAll(testdata, 0755); err != nil {
			die(err)
		}
	}
}

func snakeCase(s string) (t string) {
	if i := strings.Index(s, "Event"); i != -1 {
		s = s[:i]
	}
	for _, c := range s {
		if unicode.IsUpper(c) {
			t = t + "_" + string(unicode.ToLower(c))
		} else {
			t = t + string(c)
		}
	}
	return strings.Trim(t, "_")
}

func camelCase(s string) (t string) {
	up := true
	for _, r := range s {
		switch r {
		case ' ', '-', '_':
			up = true
		default:
			if up {
				t = t + string(unicode.ToUpper(r))
				up = false
			} else {
				t = t + string(r)
			}
		}
	}
	return idiomaticReplacer.Replace(t)
}

func scrapPayload(s *goquery.Selection, n int) string {
	url, ok := s.Find("a").Attr("href")
	if !ok {
		die("unable to find URL for scrapping")
	}
	url = "https://developer.github.com" + url
	res, err := http.Get(url)
	if err != nil {
		die(err)
	}
	defer res.Body.Close()
	doc, err := goquery.NewDocumentFromReader(res.Body)
	if err != nil {
		die(err)
	}
	var payload string
	doc.Find(`div[class='content'] > pre[class='body-response'] > code[class^='language']`).Each(
		func(i int, s *goquery.Selection) {
			if i == n {
				payload = s.Text()
			}
		},
	)
	if payload == "" {
		die(fmt.Sprintf("unable to scrap %s (n=%d)", url, n))
	}
	return payload
}

func externalJSON(event *rawEvent, s *goquery.Selection) bool {
	switch event.Name {
	case "DownloadEvent":
		event.PayloadJSON = scrapPayload(s, 1)
		return true
	case "FollowEvent":
		event.PayloadJSON = scrapPayload(s, 0)
		return true
	case "GistEvent":
		event.PayloadJSON = fmt.Sprintf(`{"action":"create","gist":%s}`, scrapPayload(s, 1))
		return true
	case "ForkApplyEvent":
		event.PayloadJSON = `{"head":"master","before":"e51831b1","after":"0c72c758c"}`
		return true
	default:
		return false
	}
}

type node struct {
	name  string
	nodes map[string]interface{}
}

var setType = func() func(*member, interface{}, string, *[]node) {
	var setType func(*member, interface{}, string, *[]node)
	setType = func(m *member, v interface{}, parent string, stack *[]node) {
		switch v := v.(type) {
		case map[string]interface{}:
			if parent == m.Name {
				// Ignore members which are named after structs to not create
				// invalid recursive types.
				break
			}
			m.Typ = m.Name
			// Files is a member of a gist object, it's handled separately since
			// it's a map.
			//
			// https://developer.github.com/v3/gists/
			if m.Typ != "Files" {
				*stack = append(*stack, node{name: m.Name, nodes: v})
			}
		case bool:
			m.Typ = "bool"
		case float64:
			m.Typ = "int"
		case []interface{}:
			if len(v) == 0 {
				m.Typ = "[]string"
				break
			}
			var prev = *m
			var cur = *m
			for _, v := range v {
				setType(&cur, v, "", stack)
				if prev.Typ != "" && cur.Typ != prev.Typ {
					die(fmt.Sprintf("heterogeneous arrays not supported: %s, %s", prev.Typ, cur.Typ))
				}
				prev = cur
			}
			m.Typ = cur.Typ
		case string:
			switch err := (&time.Time{}).UnmarshalText([]byte(v)); err {
			case nil:
				m.Typ = "time.Time"
			default:
				m.Typ = "string"
			}
		default:
			typ, ok := hardcodedTypes[m.Tag]
			if !ok {
				die(fmt.Sprintf("unable to guess type for %s: %T", m.Name, v))
			}
			m.Typ = typ
		}

	}
	return setType
}()

func linearObjects(tree map[string]interface{}) (obj []object) {
	var stack = make([]node, 0, len(tree))
	for k, v := range tree {
		v, ok := v.(map[string]interface{})
		if !ok {
			die(fmt.Sprintf("%s is not a JSON object", k))
		}
		stack = append(stack, node{name: k, nodes: v})
	}
	obj = append(obj, hardcodedFileType)
	var nd node
	for n := len(stack); n != 0; n = len(stack) {
		nd, stack = stack[n-1], stack[:n-1]
		o := object{Name: nd.name, Members: make([]member, 0, len(nd.nodes))}
		for k, v := range nd.nodes {
			// Ignore "_links" member as it's redundant and it pollutes a number
			// of structs with a "href" member.
			if k == "_links" {
				continue
			}
			m := member{Name: camelCase(k), Tag: k}
			setType(&m, v, nd.name, &stack)
			(*memberSet)(&o.Members).Add(m)
		}
		(*objectSet)(&obj).Add(o)
	}
	return obj
}

func main() {
	f, err := ioutil.TempFile(filepath.Split(output))
	if err != nil {
		die(err)
	}
	defer func() { nonil(f.Close(), os.Remove(f.Name())) }()
	res, err := http.Get(docURL)
	if err != nil {
		die(err)
	}
	defer res.Body.Close()
	doc, err := goquery.NewDocumentFromReader(res.Body)
	if err != nil {
		die(err)
	}
	var events []rawEvent
	var n int
	doc.Find(`div[class='content'] > h2[id$='event'],h3[id^='payload']+table,table+pre`).Each(
		func(i int, s *goquery.Selection) {
			switch {
			case n == len(events):
				events = append(events, rawEvent{Name: s.Text()})
			case externalJSON(&events[n], s):
				n++
			default:
				s.Find(`pre > code[class^='language']`).Each(
					func(_ int, s *goquery.Selection) {
						if events[n].PayloadJSON != "" {
							die(fmt.Sprintf("duplicate JSON payload for %q event (i=%d)", events[n].Name, i))
						}
						events[n].PayloadJSON = s.Text()
					})
				if events[n].PayloadJSON != "" {
					n++
				}
			}
		})
	for i := range events {
		switch {
		case !strings.HasSuffix(events[i].Name, "Event"):
			die(fmt.Sprintf("invalid event name: %q (i=%d)", events[i].Name, i))
		case events[i].PayloadJSON == "":
			die(fmt.Sprintf("empty payload for %q event (i=%d)", events[i].Name, i))
		}
	}
	log.SetOutput(ioutil.Discard)
	if err := tmplHeader.Execute(f, events); err != nil {
		die(err)
	}
	typeTree := make(map[string]interface{}, len(events))
	for _, event := range events {
		var v interface{}
		if err := json.Unmarshal([]byte(event.PayloadJSON), &v); err != nil {
			die(err)
		}
		typeTree[event.Name] = v
	}
	if err := tmplTypes.Execute(f, linearObjects(typeTree)); err != nil {
		die(err)
	}
	if err := nonil(f.Sync(), f.Close()); err != nil {
		die(err)
	}
	// os.Rename fails under Windows when target file exists.
	if err := nonil(os.RemoveAll(output), os.Rename(f.Name(), output)); err != nil {
		die(err)
	}
	if testdata != "" {
		for _, event := range events {
			f, err := os.OpenFile(filepath.Join(testdata, snakeCase(event.Name)+".json"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
			if err != nil {
				die(err)
			}
			_, err = io.Copy(f, strings.NewReader(event.PayloadJSON))
			if err = nonil(err, f.Sync(), f.Close()); err != nil {
				die(err)
			}
		}
	}
}
