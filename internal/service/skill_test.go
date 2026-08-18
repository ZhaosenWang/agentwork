package service

import (
	"archive/zip"
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/eushing/agentwork/internal/store"
	"github.com/eushing/agentwork/internal/zipx"
)

// TestParseSkillMarkdown: the skill name/description are read from the
// SKILL.md YAML frontmatter. A file without frontmatter, or without a name,
// is "not a skill" — returns ("", "").
func TestParseSkillMarkdown(t *testing.T) {
	cases := []struct {
		name string
		md   string
		want [2]string // name, description
	}{
		{
			"full frontmatter",
			"---\nname: code-review-checklist\ndescription: 一句话说明\n---\n正文\n",
			[2]string{"code-review-checklist", "一句话说明"},
		},
		{
			"quoted values",
			"---\nname: \"my skill\"\ndescription: 'has quotes'\n---\n",
			[2]string{"my skill", "has quotes"},
		},
		{
			"name only, no description",
			"---\nname: bare\n---\n",
			[2]string{"bare", ""},
		},
		{
			"no frontmatter",
			"# Just a markdown file\nno frontmatter here",
			[2]string{"", ""},
		},
		{
			"frontmatter without name",
			"---\ndescription: missing name\n---\n",
			[2]string{"", "missing name"},
		},
		{
			"comment lines in frontmatter",
			"---\n# a comment\nname: with-comment\ndescription: x\n---\n",
			[2]string{"with-comment", "x"},
		},
		{
			"extra frontmatter fields ignored",
			"---\nname: has-extra\ndescription: desc\nversion: 1.0\nallowed-tools:\n  - Read\n  - Bash\n---\n",
			[2]string{"has-extra", "desc"},
		},
		{
			"CRLF line endings",
			"---\r\nname: crlf-skill\r\ndescription: windows\r\n---\r\nbody\r\n",
			[2]string{"crlf-skill", "windows"},
		},
		{
			"closing delimiter at EOF (no trailing newline)",
			"---\nname: eof\n---",
			[2]string{"eof", ""},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotName, gotDesc := parseSkillMarkdown(c.md)
			if gotName != c.want[0] || gotDesc != c.want[1] {
				t.Fatalf("parseSkillMarkdown(%q) = (%q, %q), want (%q, %q)", c.md, gotName, gotDesc, c.want[0], c.want[1])
			}
		})
	}
}

// TestCreateFromZipParsesNameFromSkillMD: the platform extracts the skill
// name/description from the archive's SKILL.md, NOT from a caller-supplied
// name. A zip without SKILL.md (or whose SKILL.md has no name) is rejected
// as "not a skill package".
func TestCreateFromZipParsesNameFromSkillMD(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	svc := NewSkillService(st)

	// A valid skill zip: SKILL.md with frontmatter name + a resource file.
	zipData, err := zipx.Build(map[string]string{
		"SKILL.md":    "---\nname: code-review-checklist\ndescription: PR 审查清单\n---\n审查步骤…\n",
		"scripts/lint.sh": "echo lint\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	sk, err := svc.CreateFromZip(ctx, zipData)
	if err != nil {
		t.Fatalf("valid skill zip must create, got %v", err)
	}
	if sk.Name != "code-review-checklist" {
		t.Fatalf("name must be parsed from SKILL.md, got %q", sk.Name)
	}
	if sk.Description != "PR 审查清单" {
		t.Fatalf("description must be parsed from SKILL.md, got %q", sk.Description)
	}

	// A zip nested under a single top-level folder must still find SKILL.md.
	nestedZip, err := zipx.Build(map[string]string{
		"myskill/SKILL.md": "---\nname: nested\n---\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	sk2, err := svc.CreateFromZip(ctx, nestedZip)
	if err != nil {
		t.Fatalf("nested-folder zip must create, got %v", err)
	}
	if sk2.Name != "nested" {
		t.Fatalf("nested skill name must be parsed, got %q", sk2.Name)
	}

	// A zip WITHOUT SKILL.md → rejected as "not a skill package".
	noMD, err := zipx.Build(map[string]string{"readme.txt": "no skill here"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateFromZip(ctx, noMD); err == nil {
		t.Fatal("zip without SKILL.md must be rejected")
	}

	// A zip whose SKILL.md has NO name in frontmatter → rejected.
	noName, err := zipx.Build(map[string]string{
		"SKILL.md": "---\ndescription: no name\n---\nbody\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateFromZip(ctx, noName); err == nil {
		t.Fatal("SKILL.md without name must be rejected")
	}

	// A duplicate name is rejected (the parsed name already exists).
	if _, err := svc.CreateFromZip(ctx, zipData); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate skill name must be rejected, got %v", err)
	}
}

// TestCreateFromZipRejectsMultiSkillBundle: a zip containing MORE than one
// skill (several subdirectories each with their own SKILL.md) is rejected
// with a clear message — one zip = one skill. The user uploads skills one
// at a time.
func TestCreateFromZipRejectsMultiSkillBundle(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	svc := NewSkillService(st)

	bundle, err := zipx.Build(map[string]string{
		"skillA/SKILL.md": "---\nname: skill-a\n---\n",
		"skillB/SKILL.md": "---\nname: skill-b\n---\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.CreateFromZip(ctx, bundle)
	if err == nil {
		t.Fatal("multi-skill bundle must be rejected")
	}
	if !strings.Contains(err.Error(), "一个 zip 只能包含一个 skill") {
		t.Fatalf("multi-skill bundle must be rejected with the one-skill message, got %v", err)
	}
	// Neither skill was persisted.
	var n int
	_ = st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM skill`).Scan(&n)
	if n != 0 {
		t.Fatalf("rejected bundle must not persist any skill, got %d rows", n)
	}
}

// TestCreateFromTextBlockParsesNameFromSkillMD: the legacy text-block
// (JSON) upload path ALSO parses name/description from SKILL.md.
func TestCreateFromTextBlockParsesNameFromSkillMD(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	svc := NewSkillService(st)

	sk, err := svc.Create(ctx, map[string]string{
		"SKILL.md": "---\nname: textblock-skill\ndescription: via json\n---\n",
	})
	if err != nil {
		t.Fatalf("text-block create must succeed, got %v", err)
	}
	if sk.Name != "textblock-skill" {
		t.Fatalf("name must be parsed from SKILL.md, got %q", sk.Name)
	}

	// Text block without SKILL.md → rejected.
	if _, err := svc.Create(ctx, map[string]string{"x.txt": "no skill"}); err == nil {
		t.Fatal("text block without SKILL.md must be rejected")
	}
}

// TestCreateFromZipNormalizesToFlatArchive: regardless of whether the
// uploaded zip had a top-level skill/ wrapper folder, the STORED
// package.zip is FLAT (SKILL.md + files at the archive root, no wrapper).
// This is the platform's normalization contract — every consumer
// (config.push → CLI Extract) sees one format and never re-detects a
// wrapper directory.
func TestCreateFromZipNormalizesToFlatArchive(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	svc := NewSkillService(st)

	// A NESTED zip: top-level myskill/ folder wrapping SKILL.md + a script.
	nested, err := zipx.Build(map[string]string{
		"myskill/SKILL.md":      "---\nname: nested-flat\ndescription: x\n---\n",
		"myskill/scripts/lint.sh": "echo lint\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	sk, err := svc.CreateFromZip(ctx, nested)
	if err != nil {
		t.Fatalf("nested zip must create, got %v", err)
	}

	// The stored package.zip must be FLAT: SKILL.md at the root, no
	// myskill/ wrapper. Read it back and check its entry names.
	archive, err := svc.PackageZip(ctx, sk.ID)
	if err != nil {
		t.Fatalf("read back package.zip: %v", err)
	}
	entries, err := zipxEntries(archive)
	if err != nil {
		t.Fatalf("read archive entries: %v", err)
	}
	hasSkillMD := false
	for _, e := range entries {
		if e == "SKILL.md" {
			hasSkillMD = true
		}
		if strings.HasPrefix(e, "myskill/") {
			t.Fatalf("stored archive must be flat (no wrapper), found %q", e)
		}
	}
	if !hasSkillMD {
		t.Fatalf("stored archive must have SKILL.md at root, got %v", entries)
	}

	// A FLAT zip (no wrapper) must also stay flat.
	flat, err := zipx.Build(map[string]string{
		"SKILL.md":      "---\nname: already-flat\n---\n",
		"scripts/x.sh":  "echo x\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	sk2, err := svc.CreateFromZip(ctx, flat)
	if err != nil {
		t.Fatalf("flat zip must create, got %v", err)
	}
	archive2, _ := svc.PackageZip(ctx, sk2.ID)
	entries2, _ := zipxEntries(archive2)
	hasSkillMD = false
	for _, e := range entries2 {
		if e == "SKILL.md" {
			hasSkillMD = true
		}
	}
	if !hasSkillMD {
		t.Fatalf("flat zip must stay flat with SKILL.md at root, got %v", entries2)
	}
}

// zipxEntries reads a zip's entry names (for the normalization test).
func zipxEntries(data []byte) ([]string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		out = append(out, f.Name)
	}
	return out, nil
}


