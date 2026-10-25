package keysenv

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	f, err := Parse(strings.NewReader(`
PROXY_KEYS=sk-a,sk-b      # client keys
OPENAI_1_KEY=sk-oa-1
OPENAI_TEMP3_KEY=sk-oa-3
GOOGLE_AI_STUDIO_TEMP2_KEY=AIza-x
LOCAL_GPU_KEY="x"
RANDOM_UNRELATED=1
`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(f.ProxyKeys), 2; got != want {
		t.Fatalf("proxy keys = %d, want %d", got, want)
	}
	if f.ProxyKeys[0] != "sk-a" || f.ProxyKeys[1] != "sk-b" {
		t.Errorf("proxy keys = %v", f.ProxyKeys)
	}
	want := map[string]string{
		"OPENAI_1_KEY":               "sk-oa-1",
		"OPENAI_TEMP3_KEY":           "sk-oa-3",
		"GOOGLE_AI_STUDIO_TEMP2_KEY": "AIza-x",
		"LOCAL_GPU_KEY":              "x",
	}
	for k, v := range want {
		if f.Keys[k] != v {
			t.Errorf("Keys[%s] = %q, want %q", k, f.Keys[k], v)
		}
	}
	if _, ok := f.Keys["PROXY_KEYS"]; ok {
		t.Errorf("PROXY_KEYS must not appear in Keys")
	}
}

func TestParseMissingProxyKeysStillParses(t *testing.T) {
	f, err := Parse(strings.NewReader("OPENAI_1_KEY=sk-oa-1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.ProxyKeys) != 0 {
		t.Errorf("proxy keys = %v, want empty", f.ProxyKeys)
	}
}

func TestParseRejectsBrokenLine(t *testing.T) {
	if _, err := Parse(strings.NewReader("JUST_A_NAME\n")); err == nil {
		t.Fatal("expected an error for a line without '='")
	}
}
