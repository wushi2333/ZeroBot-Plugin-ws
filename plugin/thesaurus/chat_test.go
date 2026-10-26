package thesaurus

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBlocklist(t *testing.T) {
	p := filepath.Join(t.TempDir(), "kimoi_block.json")
	b := &blocklist{path: p}
	if b.has("啊~") {
		t.Fatal("missing file blocks nothing")
	}
	write := func(s string, mod time.Time) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Now().Add(-time.Hour)
	write(`["啊~", "  上山打老虎，老虎没打到\n咱来凑数  "]`, t0)
	if !b.has("啊~") || !b.has(" 啊~ ") {
		t.Fatal("exact reply (ignoring surrounding spaces) must be blocked")
	}
	if !b.has("上山打老虎，老虎没打到 咱来凑数") {
		t.Fatal("whitespace differences must not matter")
	}
	if b.has("啊") || b.has("啊~~") {
		t.Fatal("only exact replies are blocked, not substrings")
	}
	// 改坏文件: 保留原来的列表
	write(`["啊~",`, t0.Add(time.Minute))
	if !b.has("啊~") {
		t.Fatal("a broken file must keep the previous list")
	}
	// 改好之后自动生效
	write(`["草"]`, t0.Add(2*time.Minute))
	if b.has("啊~") || !b.has("草") {
		t.Fatal("edits must take effect without restart")
	}
	// 对象格式: 整句 + 屏蔽词
	write(`{"replies": ["草"], "words": ["爸爸", "爹", " "]}`, t0.Add(3*time.Minute))
	if !b.has("草") || !b.has("你又不是咱的爸爸……") || !b.has("叫爹！") {
		t.Fatal("object format must block exact replies and any reply containing a word")
	}
	if b.has("爸妈都在家") || b.has("好的哦~") {
		t.Fatal("unrelated replies must pass; blank words are ignored")
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if b.has("草") || b.has("爸爸") {
		t.Fatal("deleting the file unblocks everything")
	}
}
