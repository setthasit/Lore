// Command scripted is a plugin whose behaviour comes entirely from a script, so
// a test can build one that misbehaves in exactly one way. The script is
// "script.txt" beside this binary: blank-line-separated groups of
// "<op> <action> [argument]" lines, and a request runs the first unused group
// naming its op. The actions are the cases in run, the placeholders in expand.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type request struct {
	V       int               `json:"v"`
	ID      string            `json:"id"`
	Op      string            `json:"op"`
	Secrets map[string]string `json:"secrets"`
	Cursor  map[string]string `json:"cursor"`
	Config  map[string]any    `json:"config"`
	Texts   []string          `json:"texts"`
	Path    string            `json:"path"`
}

type step struct {
	action string
	arg    string
}

type group struct {
	op    string
	steps []step
	used  bool
}

func main() {
	groups, err := script()
	if err != nil {
		fmt.Fprintln(os.Stderr, "scripted:", err)
		os.Exit(2)
	}

	out := bufio.NewWriter(os.Stdout)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64<<10), 8<<20)

	for in.Scan() {
		var req request
		if err := json.Unmarshal(in.Bytes(), &req); err != nil {
			fmt.Fprintln(os.Stderr, "scripted: unreadable request:", err)
			os.Exit(3)
		}

		answered := false
		for i := range groups {
			if groups[i].used || groups[i].op != req.Op {
				continue
			}
			groups[i].used = true
			answered = true
			for _, s := range groups[i].steps {
				run(out, s, req)
			}
			break
		}
		if !answered {
			fmt.Fprintln(os.Stderr, "scripted: no script left for op", req.Op)
			os.Exit(4)
		}
	}
	_ = out.Flush()
}

func run(out *bufio.Writer, s step, req request) {
	switch s.action {
	case "emit", "raw":
		fmt.Fprintln(out, expand(s.arg, req))
		_ = out.Flush()
	case "bigline":
		size, _ := strconv.Atoi(s.arg)
		fmt.Fprintf(out, `{"v":1,"id":%q,"ok":true,"text":%q}`+"\n", req.ID, strings.Repeat("x", size))
		_ = out.Flush()
	case "stderr":
		fmt.Fprintln(os.Stderr, expand(s.arg, req))
	case "partial":
		fmt.Fprint(os.Stderr, expand(s.arg, req))
	case "sleep":
		ms, _ := strconv.Atoi(s.arg)
		time.Sleep(time.Duration(ms) * time.Millisecond)
	case "exit":
		code, _ := strconv.Atoi(s.arg)
		_ = out.Flush()
		os.Exit(code)
	default:
		fmt.Fprintln(os.Stderr, "scripted: unknown action", s.action)
		os.Exit(5)
	}
}

func expand(text string, req request) string {
	text = strings.ReplaceAll(text, "$ID", req.ID)
	text = strings.ReplaceAll(text, "$OP", req.Op)
	text = strings.ReplaceAll(text, "$NTEXTS", strconv.Itoa(len(req.Texts)))
	text = strings.ReplaceAll(text, "$PID", strconv.Itoa(os.Getpid()))
	// A Windows clone root arrives full of backslashes, which no JSON string can carry raw.
	text = strings.ReplaceAll(text, "$PATH", escape(req.Path))

	for _, ref := range []struct {
		prefix string
		lookup func(string) string
	}{
		{"$ENV{", os.Getenv},
		{"$SECRET{", func(key string) string { return req.Secrets[key] }},
		{"$CURSOR{", func(key string) string { return req.Cursor[key] }},
		{"$CONFIG{", func(key string) string { return fmt.Sprint(req.Config[key]) }},
	} {
		for {
			start := strings.Index(text, ref.prefix)
			if start < 0 {
				break
			}
			end := strings.Index(text[start:], "}")
			if end < 0 {
				break
			}
			key := text[start+len(ref.prefix) : start+end]
			text = text[:start] + ref.lookup(key) + text[start+end+1:]
		}
	}
	return text
}

func escape(value string) string {
	quoted, err := json.Marshal(value)
	if err != nil {
		return value
	}
	return string(quoted[1 : len(quoted)-1])
}

func script() ([]group, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(self), "script.txt"))
	if err != nil {
		return nil, err
	}

	var groups []group
	fresh := true
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			fresh = true
			continue
		}
		op, rest, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("script line %q names no action", line)
		}
		action, arg, _ := strings.Cut(rest, " ")

		if fresh || groups[len(groups)-1].op != op {
			groups = append(groups, group{op: op})
			fresh = false
		}
		last := &groups[len(groups)-1]
		last.steps = append(last.steps, step{action: action, arg: arg})
	}
	return groups, nil
}
