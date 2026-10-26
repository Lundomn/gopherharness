package app

import (
	"bytes"
	"strings"
	"testing"
)

func TestNativeStreamDisplaysWithoutTextToolTags(t *testing.T) {
	var out bytes.Buffer
	s := &finalStreamOutput{out: &out, native: true}
	s.Start()
	if e := s.Delta("读取完成，"); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), "读取完成，") {
		t.Fatal("native delta hidden")
	}
	if e := s.Delta("检查通过。"); e != nil {
		t.Fatal(e)
	}
	if !s.Finish("读取完成，检查通过。") {
		t.Fatal("stream not displayed")
	}
	if strings.Count(out.String(), "读取完成") != 1 {
		t.Fatal("duplicated final", out.String())
	}
}
