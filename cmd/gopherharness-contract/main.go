package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/protocol"
)

type contractCase struct {
	Name string `json:"name"`
	Raw  string `json:"raw"`
}

type contractResult struct {
	Name  string           `json:"name"`
	Kind  string           `json:"kind"`
	Tools []model.ToolCall `json:"tools,omitempty"`
	Text  string           `json:"text,omitempty"`
}

func main() {
	var cases []contractCase
	if err := json.NewDecoder(os.Stdin).Decode(&cases); err != nil {
		fmt.Fprintln(os.Stderr, "decode contract input:", err)
		os.Exit(2)
	}
	results := make([]contractResult, 0, len(cases))
	for _, testCase := range cases {
		parsed := protocol.Parse(testCase.Raw)
		results = append(results, contractResult{Name: testCase.Name, Kind: string(parsed.Kind), Tools: parsed.Tools, Text: parsed.Text})
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(results); err != nil {
		fmt.Fprintln(os.Stderr, "encode contract output:", err)
		os.Exit(2)
	}
}
