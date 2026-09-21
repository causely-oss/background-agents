// Command causely-background-agent runs the reference investigation agent.
// See ../../README.md for what it does; all of the actual logic lives in
// internal/agent.
package main

import "github.com/causely-oss/background-agents/causely-background-agent/internal/agent"

func main() {
	agent.Run()
}
