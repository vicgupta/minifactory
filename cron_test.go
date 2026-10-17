package main

import (
	"strings"
	"testing"
)

func TestValidCronSchedule(t *testing.T) {
	for _, ok := range []string{"* * * * *", "*/5 * * * *", "0 9 * * 1"} {
		if !validCronSchedule(ok) {
			t.Errorf("validCronSchedule(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "* * * *", "* * * * * *", "every minute", "* *"} {
		if validCronSchedule(bad) {
			t.Errorf("validCronSchedule(%q) = true, want false", bad)
		}
	}
}

func TestCronBlock(t *testing.T) {
	b := cronBlock("* * * * *", "/home/u/.local/bin/minifactory", "/home/u/.minifactory")
	for _, want := range []string{
		cronMarker,
		"MINIFACTORY_DIR=/home/u/.minifactory",
		"* * * * * flock -n /home/u/.minifactory/cycle.lock",
		"/home/u/.local/bin/minifactory run-once",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("cronBlock missing %q:\n%s", want, b)
		}
	}
	if !strings.HasSuffix(b, "\n") {
		t.Errorf("cronBlock should end with newline:\n%s", b)
	}
}

func TestCronBlockQuotesSpaces(t *testing.T) {
	b := cronBlock("* * * * *", "/home/my user/bin/mf", "/home/my user/mf")
	if !strings.Contains(b, "'/home/my user/bin/mf' run-once") {
		t.Errorf("exe path with spaces not quoted:\n%s", b)
	}
}

func TestUpsertCronBlockAppend(t *testing.T) {
	current := "0 9 * * * /usr/bin/backup\n"
	block := cronBlock("* * * * *", "/bin/mf", "/d")
	out := upsertCronBlock(current, block)
	if !strings.HasPrefix(out, current) {
		t.Errorf("existing crontab not preserved:\n%s", out)
	}
	if !strings.HasSuffix(out, block) {
		t.Errorf("block not appended:\n%s", out)
	}
	if strings.Count(out, cronMarker) != 1 {
		t.Errorf("want exactly one marker:\n%s", out)
	}
}

func TestUpsertCronBlockReplace(t *testing.T) {
	old := cronBlock("*/5 * * * *", "/bin/old", "/d")
	current := "0 9 * * * /usr/bin/backup\n" + old
	block := cronBlock("* * * * *", "/bin/new", "/d")
	out := upsertCronBlock(current, block)
	if strings.Count(out, cronMarker) != 1 {
		t.Errorf("want exactly one marker after replace:\n%s", out)
	}
	if strings.Contains(out, "/bin/old") || strings.Contains(out, "*/5 *") {
		t.Errorf("old block not replaced:\n%s", out)
	}
	if !strings.Contains(out, "/bin/new") || !strings.Contains(out, "0 9 * * * /usr/bin/backup") {
		t.Errorf("new block or unrelated entry missing:\n%s", out)
	}
}

func TestRemoveCronBlock(t *testing.T) {
	block := cronBlock("* * * * *", "/bin/mf", "/d")
	current := "0 9 * * * /usr/bin/backup\n" + block
	out, found := removeCronBlock(current)
	if !found {
		t.Fatal("removeCronBlock: not found")
	}
	if strings.Contains(out, cronMarker) {
		t.Errorf("marker still present:\n%s", out)
	}
	if !strings.Contains(out, "0 9 * * * /usr/bin/backup") {
		t.Errorf("unrelated entry dropped:\n%s", out)
	}
	if _, found := removeCronBlock("0 9 * * * /usr/bin/backup\n"); found {
		t.Error("removeCronBlock: reported found with no block present")
	}
}

func TestScheduleDesc(t *testing.T) {
	if got := scheduleDesc("* * * * *"); got != "every minute" {
		t.Errorf("got %q", got)
	}
	if got := scheduleDesc("*/5 * * * *"); got != "every 5 minutes" {
		t.Errorf("got %q", got)
	}
}
