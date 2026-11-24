// internal/nfo/build_test.go
package nfo

import (
	"strings"
	"testing"
)

// crlf переводит фикстуру, написанную с \n, в CRLF — так файлы пишет MediaElch.
func crlf(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

const ratingsBlockLF = `    <ratings>
        <rating name="imdb" max="10" default="true">
            <value>7.9</value>
            <votes>274784</votes>
        </rating>
    </ratings>
`

func movieNFO(ratings string) string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<movie>
    <title>Test</title>
    <originaltitle>Test</originaltitle>
` + ratings + `    <userrating>0</userrating>
    <year>2023</year>
</movie>
`
}

var freshEntries = []RatingEntry{{Source: "imdb", Value: "7.9", Votes: 276747, Default: true}}

// Обновлённый рейтинг должен заменять старый блок, а не вставляться рядом,
// и в CRLF-файле, и в LF-файле.
func TestApplyRatingEntriesReplacesBlock(t *testing.T) {
	cases := map[string]string{
		"lf":   movieNFO(ratingsBlockLF),
		"crlf": crlf(movieNFO(ratingsBlockLF)),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			out, changed, err := ApplyRatingEntries(in, freshEntries)
			if err != nil {
				t.Fatal(err)
			}
			if !changed {
				t.Fatal("expected change")
			}
			if n := strings.Count(out, "<ratings>"); n != 1 {
				t.Fatalf("expected 1 ratings block, got %d:\n%s", n, out)
			}
			if !strings.Contains(out, "<votes>276747</votes>") {
				t.Fatalf("fresh votes missing:\n%s", out)
			}
			// Второй прогон с теми же данными — файл не трогаем.
			if _, changed, _ := ApplyRatingEntries(out, freshEntries); changed {
				t.Fatal("second run with same data must not change file")
			}
		})
	}
}

// В CRLF-файле не должно появляться строк с голым \n.
func TestApplyRatingEntriesKeepsCRLF(t *testing.T) {
	out, _, err := ApplyRatingEntries(crlf(movieNFO(ratingsBlockLF)), freshEntries)
	if err != nil {
		t.Fatal(err)
	}
	if bare := strings.Count(out, "\n") - strings.Count(out, "\r\n"); bare != 0 {
		t.Fatalf("found %d LF-only line endings in CRLF file", bare)
	}
}

// Файл, уже испорченный прежней ошибкой: несколько блоков подряд, причём
// первый совпадает со свежими данными. Должен остаться один блок.
func TestApplyRatingEntriesCollapsesDuplicates(t *testing.T) {
	newest := strings.Replace(ratingsBlockLF, "274784", "276747", 1)
	in := crlf(movieNFO(newest + ratingsBlockLF + ratingsBlockLF))
	out, changed, err := ApplyRatingEntries(in, freshEntries)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("duplicates present: expected rewrite even though first block matches")
	}
	if n := strings.Count(out, "<ratings>"); n != 1 {
		t.Fatalf("expected 1 ratings block, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "<userrating>0</userrating>") {
		t.Fatal("neighbouring tag lost")
	}
}
