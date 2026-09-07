package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestGoldenModelOutputContract(t *testing.T) {
	var cases []struct {
		Name     string         `json:"name"`
		Raw      string         `json:"raw"`
		Expected map[string]any `json:"expected"`
	}
	path := filepath.Join("..", "..", "contracts", "model_output_cases.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range cases {
		t.Run(testCase.Name, func(t *testing.T) {
			parsed := Parse(testCase.Raw)
			actual := map[string]any{"kind": string(parsed.Kind)}
			if len(parsed.Tools) > 0 {
				raw, _ := json.Marshal(parsed.Tools)
				var tools any
				_ = json.Unmarshal(raw, &tools)
				actual["tools"] = tools
			}
			if parsed.Text != "" {
				actual["text"] = parsed.Text
			}
			if !reflect.DeepEqual(actual, testCase.Expected) {
				t.Fatalf("actual=%#v expected=%#v", actual, testCase.Expected)
			}
		})
	}
}
