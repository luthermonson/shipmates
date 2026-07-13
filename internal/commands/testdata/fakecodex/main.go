package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func main() {
	if len(os.Args) != 3 || os.Args[1] != "app-server" || os.Args[2] != "--stdio" {
		os.Exit(2)
	}
	cwd, _ := os.Getwd()
	thread := "thread-" + filepath.Base(cwd)
	in := bufio.NewScanner(os.Stdin)
	reply := func(id json.RawMessage, result any) {
		b, _ := json.Marshal(map[string]any{"id": id, "result": result})
		fmt.Println(string(b))
	}
	for in.Scan() {
		var req request
		if json.Unmarshal(in.Bytes(), &req) != nil {
			os.Exit(3)
		}
		switch req.Method {
		case "initialize":
			reply(req.ID, map[string]string{"userAgent": "codex-cli 0.144.1"})
		case "initialized":
		case "thread/start":
			reply(req.ID, map[string]any{"thread": map[string]string{"id": thread}})
		case "thread/resume":
			reply(req.ID, map[string]any{"thread": map[string]string{"id": thread}})
		case "turn/start":
			reply(req.ID, map[string]any{"turn": map[string]string{"id": "turn-1"}})
		case "turn/steer":
			reply(req.ID, map[string]bool{"accepted": true})
			fmt.Printf("{\"method\":\"turn/completed\",\"params\":{\"threadId\":%q,\"turn\":{\"id\":\"turn-1\"}}}\n", thread)
		case "turn/interrupt":
			reply(req.ID, map[string]bool{"accepted": true})
		default:
			os.Exit(4)
		}
	}
}
