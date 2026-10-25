package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Band is a frequency range of interest; labels are shown in the dashboard.
type Band struct {
	Name   string // Czech label
	LoMHz  float64
	HiMHz  float64
	NameEN string // English label (falls back to Name when empty)
}

// Built-in band plan, used unless -bands points to a file.
// First match wins, so narrower bands go before the ones that contain them.
var bands = []Band{
	{"CB 27 MHz", 26.965, 27.405, "CB 27 MHz"},
	{"10 m", 28, 29.7, "10 m"},
	{"6 m", 50, 54, "6 m"},
	{"4 m", 70, 70.5, "4 m"},
	{"FM rozhlas", 87.5, 108, "FM broadcast"},
	{"Letecké (navigace)", 108, 118, "Airband (navigation)"},
	{"Letecké (hlas)", 118, 137, "Airband (voice)"},
	{"Meteo družice", 137, 138, "Weather satellites"},
	{"2 m", 144, 146, "2 m"},
	{"Námořní VHF", 156, 162.05, "Marine VHF"},
	{"DAB", 174, 240, "DAB"},
	{"Vojenské letecké UHF", 225, 400, "Military airband UHF"},
	{"Radiosondy", 400, 406, "Radiosondes"},
	{"ISM 433", 433.05, 434.79, "ISM 433"},
	{"70 cm", 430, 440, "70 cm"},
	{"PMR446", 446, 446.2, "PMR446"},
	{"LTE 800", 791, 862, "LTE 800"},
	{"ISM 868", 863, 870, "ISM 868"},
	{"GSM-R", 918, 925, "GSM-R"},
	{"GSM 900 (downlink)", 925, 960, "GSM 900 (downlink)"},
	{"ADS-B 1090", 1089, 1091, "ADS-B 1090"},
	{"23 cm", 1240, 1300, "23 cm"},
}

var otherBand = Band{Name: "Ostatní", NameEN: "Other"}

// LoadBands replaces the built-in band plan with one read from a text file.
// Format: one band per line, "name; low MHz; high MHz[; English name]";
// '#' starts a comment.
func LoadBands(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var out []Band
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		parts := strings.Split(line, ";")
		if len(parts) != 3 && len(parts) != 4 {
			return fmt.Errorf("%s:%d: expected \"name; low; high[; English name]\"", path, n)
		}
		lo, err1 := strconv.ParseFloat(strings.TrimSpace(strings.ReplaceAll(parts[1], ",", ".")), 64)
		hi, err2 := strconv.ParseFloat(strings.TrimSpace(strings.ReplaceAll(parts[2], ",", ".")), 64)
		if err1 != nil || err2 != nil || hi <= lo {
			return fmt.Errorf("%s:%d: bad frequency range", path, n)
		}
		b := Band{Name: strings.TrimSpace(parts[0]), LoMHz: lo, HiMHz: hi}
		if len(parts) == 4 {
			b.NameEN = strings.TrimSpace(parts[3])
		}
		out = append(out, b)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if len(out) == 0 {
		return fmt.Errorf("%s: no bands defined", path)
	}
	bands = out
	return nil
}

func bandOf(hz int64) Band {
	f := float64(hz) / 1e6
	for _, b := range bands {
		if f >= b.LoMHz && f < b.HiMHz {
			return b
		}
	}
	return otherBand
}

// Label returns the band name in the requested language ("cs" or "en").
func (b Band) Label(lang string) string {
	if lang == "en" && b.NameEN != "" {
		return b.NameEN
	}
	return b.Name
}

// bandLabel is a shortcut for bandOf(hz).Label(lang).
func bandLabel(hz int64, lang string) string { return bandOf(hz).Label(lang) }

// bandsMatching returns bands whose name contains q (case-insensitive).
func bandsMatching(q string) []Band {
	q = strings.ToLower(q)
	var out []Band
	for _, b := range bands {
		if strings.Contains(strings.ToLower(b.Name), q) || strings.Contains(strings.ToLower(b.NameEN), q) {
			out = append(out, b)
		}
	}
	return out
}
