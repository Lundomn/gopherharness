package runtime

import (
	"fmt"
	"os"

	"github.com/Lundomn/gopherharness/internal/model"
)

func (a *Agent) preflightPath(call model.ToolCall) error {
	switch call.Name {
	case "read_file", "write_file", "patch_file", "list_files", "search", "inspect_image":
		rel, _ := call.Args["path"].(string)
		p, err := a.Resolve(rel)
		if err != nil {
			return err
		}
		if call.Name == "patch_file" || call.Name == "write_file" {
			_, statErr := os.Stat(p)
			if statErr == nil && !a.FreshRead(p) {
				return fmt.Errorf("read current file before modifying %s", rel)
			}
			if statErr != nil && !os.IsNotExist(statErr) {
				return statErr
			}
		}
	}
	return nil
}
