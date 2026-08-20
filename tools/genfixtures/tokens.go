package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/townsendmerino/aikit/embed"
)

// A WIDE tokenizer gate.
//
// The vector fixtures cover sixteen cases, which is enough to catch a
// broken forward pass but thin for a tokenizer: a Viterbi that is wrong
// only on, say, digit runs or mixed-script words would sail through
// them. Ids are cheap to generate and cheap to compare, so this corpus
// is deliberately large and awkward.
//
// No vectors here — just text and ids.

type tokenCase struct {
	Text string  `json:"text"`
	IDs  []int32 `json:"ids"`
}

var scripts = []string{
	"the user prefers TOML over YAML", "Yorkshire", "sourdough starter",
	"l'utilisateur est allergique aux fruits de mer", "château d'eau", "œuf",
	"der Benutzer ist allergisch gegen Meeresfrüchte", "Straße", "Größe",
	"用户对贝类过敏", "北京市朝阳区", "这是一个测试",
	"ユーザーは甲殻類にアレルギーがあります", "東京都渋谷区", "テスト",
	"пользователь живёт в Йоркшире", "Москва", "Ёлка",
	"المستخدم يعيش في يوركشاير", "مرحبا بالعالم",
	"ο χρήστης ζει στο Γιορκσάιρ", "Ελλάδα",
	"사용자는 요크셔에 산다", "안녕하세요",
	"वह यॉर्कशायर में रहता है", "नमस्ते",
	"אני גר ביורקשייר", "שלום",
	"kullanıcı Yorkshire'da yaşıyor", "İstanbul", "ığüşöç",
	"người dùng sống ở Yorkshire", "Tiếng Việt",
	"ผู้ใช้อาศัยอยู่ในยอร์กเชียร์",
}

var awkward = []string{
	"", " ", "  ", "\t", "\n", " leading", "trailing ", "  both  ",
	"a", "ab", "abc", "I", "0", "1", "42", "3.14159", "1,000,000",
	"2026-08-20", "10:30", "50%", "$100", "£99.99", "€50", "¥1000",
	"e-mail", "co-operate", "don't", "can't", "it's", "O'Brien",
	"CamelCase", "snake_case", "kebab-case", "SCREAMING_SNAKE",
	"supercalifragilisticexpialidocious", "antidisestablishmentarianism",
	"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "zzzzzzzzzz",
	"https://example.com/path?q=1&r=2", "user@example.com",
	"/usr/local/bin", "C:\\Windows\\System32", "~/.config/lobslaw",
	"func main() { fmt.Println(\"hi\") }", "SELECT * FROM users;",
	"<html><body>x</body></html>", "{\"key\": \"value\"}",
	"🍤", "🍤 shellfish 🚨", "👨‍👩‍👧‍👦", "🇬🇧", "café ☕",
	"...", "!!!", "?!", "--", "—", "…", "«guillemets»", "\"quotes\"",
	"ﬁ", "ﬂ", "Ⅷ", "½", "①", "Ａ", "ＡＢＣ", "１２３",
	"ａｂｃ ＡＢＣ", "㍿", "㌀",
	"mixed中文and English", "Русскийand中文", "emoji🍤inside",
	"a\u0301", "e\u0301", "o\u0308", "n\u0303",
	"\u200bzero width", "soft\u00adhyphen", "non\u00a0breaking",

	// Runs of characters absent from a 250k vocabulary, which is the
	// ONLY way to reach fuse_unk: consecutive unknown pieces must
	// collapse to a single <unk>, not one per character. Without these
	// the corpus never exercised it and dropping the fusion passed
	// every case.
	"\U000F0000\U000F0001\U000F0002",   // private use, plane 15
	"\U00010000\U00010001",             // Linear B
	"\U000130B8\U000130B9",             // Egyptian hieroglyphs
	"\U000F0000 real words \U000F0001", // unknown either side of known
}

func genTokens(tok *embed.Tokenizer, out string) error {
	cases := make([]tokenCase, 0, len(scripts)+len(awkward))
	for _, s := range append(append([]string{}, scripts...), awkward...) {
		cases = append(cases, tokenCase{Text: s, IDs: tok.Encode(s)})
	}
	path := filepath.Join(out, "tokens.json")
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	enc := json.NewEncoder(f)
	enc.SetIndent("", " ")
	if err := enc.Encode(cases); err != nil {
		return err
	}
	fmt.Printf("wrote %s: %d token cases\n", path, len(cases))
	return nil
}
