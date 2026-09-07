package protocol

import "testing"

func TestParseJSONToolAndArgumentsAlias(t *testing.T) {
	for _, raw := range []string{`<tool>{"name":"read_file","args":{"path":"README.md"}}</tool>`, `<tool>{"name":"read_file","arguments":{"path":"README.md"}}</tool>`} {
		p := Parse(raw)
		if p.Kind != KindTools || len(p.Tools) != 1 {
			t.Fatalf("unexpected parse: %#v", p)
		}
		if p.Tools[0].Args["path"] != "README.md" {
			t.Fatalf("args lost: %#v", p.Tools[0].Args)
		}
	}
}
func TestParseXMLWriteAndFinal(t *testing.T) {
	p := Parse(`<tool name="write_file" path="x.txt"><content>hello</content></tool>`)
	if p.Tools[0].Args["content"] != "hello" {
		t.Fatalf("content mismatch: %#v", p)
	}
	f := Parse(`<final>done</final>`)
	if f.Kind != KindFinal || f.Text != "done" {
		t.Fatalf("final mismatch: %#v", f)
	}
}
func TestParseMalformedReturnsRetry(t *testing.T) {
	p := Parse(`hello`)
	if p.Kind != KindRetry {
		t.Fatalf("expected retry: %#v", p)
	}
}

func TestParseJSONToolList(t *testing.T) {
	p := Parse(`<tool>[{"name":"read_file","args":{"path":"a"}},{"name":"search","args":{"pattern":"x"}}]</tool>`)
	if p.Kind != KindTools || len(p.Tools) != 2 || p.Tools[1].Name != "search" {
		t.Fatalf("unexpected parse: %#v", p)
	}
}
