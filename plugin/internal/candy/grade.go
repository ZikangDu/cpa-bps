package candy

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	StatusPass       = "pass"
	StatusAnswerFail = "answer_fail"
	// MaxAnswerBytes bounds parsing work before any normalization or allocation.
	MaxAnswerBytes = 64 * 1024
)

type Grade struct {
	Status string `json:"status"`
	Answer string `json:"answer,omitempty"`
	Reason string `json:"reason,omitempty"`
}

var (
	integerPattern             = regexp.MustCompile(`^[0-9]{1,9}$`)
	shortPrefix                = regexp.MustCompile(`(?i)^(?:(?:最终|最后)?(?:答案|结果)[ \t]*(?:是|为)?[ \t]*[:=]?|(?:最少|至少)(?:需要)?(?:取出|抽取|摸出)?|(?:需要|取出|抽取|摸出)|(?:the[ \t]+)?(?:final[ \t]+)?answer[ \t]*(?:is|[:=])?|at[ \t]+least)[ \t]*`)
	quantityPattern            = regexp.MustCompile(`(?i)^([0-9]{1,9}|二十一)[ \t]*(?:(?:颗|个|粒)[ \t]*)?(?:糖果|糖|candies)?[ \t]*(?:即可)?[。.!！]?$`)
	declarationPattern         = regexp.MustCompile(`(?i)(?:最终答案|最后答案|答案|最终结果|最后结果|(?:final[ \t]+)?answer)[ \t]*(?:是|为|is|[:=])?[ \t]*(?:\*\*|__|` + "`" + `)?([+−-]?[0-9]+|二十一)`)
	alternativePattern         = regexp.MustCompile(`(?i)^[ \t]*(?:(?:颗|个|粒)?(?:糖果|糖|candies)?)?[ \t]*(?:或(?:者)?|or|and|和|与|、|/|,|，|[-~～–—]|到|至)[ \t]*(?:[0-9]|二十一)`)
	bareAlternativesPattern    = regexp.MustCompile(`(?i)^(?:[0-9]+|二十一)[ \t]*(?:颗|个|粒)?(?:糖果|糖|candies)?[ \t]*(?:或(?:者)?|or|and|和|与|、|/|,|，|[-~～–—]|到|至)[ \t]*(?:[0-9]+|二十一)(?:[ \t]*(?:颗|个|粒)?(?:糖果|糖|candies)?)?[。.!！]?$`)
	numericContinuationPattern = regexp.MustCompile(`^(?:\.[0-9]|[eE][+-]?[0-9]|[ \t]+[0-9])`)
	metadataPattern            = regexp.MustCompile(`(?i)(?:标准答案|参考答案|expected[ \t]+answer|ground[ \t]+truth)[ \t]*(?:是|为|is|[:=])?[ \t]*(?:\*\*|__|` + "`" + `)?(?:[0-9]|二十一)`)
)

// ParseGrade accepts a complete short answer, or a short answer on the final
// nonempty line. Earlier prose and calculations are not candidate answers.
// Explicit answer declarations elsewhere must agree; conflicting or multiple
// candidates fail closed. A bare occurrence of the expected integer never
// suffices. Reason is a fixed code suitable for storage, not model-controlled
// text. Callers handle transport errors separately and must not score them.
func ParseGrade(answer, expected string) Grade {
	fail := func(reason string) Grade { return Grade{Status: StatusAnswerFail, Reason: reason} }
	if len(answer) > MaxAnswerBytes {
		return fail("answer_too_long")
	}
	if len(expected) > 64 {
		return fail("invalid_expected_answer")
	}
	normalizedExpected, ok := normalize(expected)
	if !ok {
		return fail("invalid_expected_answer")
	}
	normalizedExpected, ok = decimal(strings.TrimSpace(normalizedExpected))
	if !ok {
		return fail("invalid_expected_answer")
	}
	normalized, ok := normalize(answer)
	if !ok {
		return fail("invalid_answer_encoding")
	}
	normalized = strings.TrimSpace(normalized)
	if normalized == "" {
		return fail("empty_answer")
	}
	// A model cannot redefine the local reference by quoting a purported
	// expected/standard answer. Such metadata is not an answer declaration.
	if metadataPattern.MatchString(normalized) {
		return fail("ambiguous_reference_answer")
	}
	lines := strings.Split(normalized, "\n")
	finalAnswer, ok := parseShortLine(lines[len(lines)-1])
	if !ok {
		return fail("no_unique_final_answer")
	}
	for _, line := range lines {
		if bareAlternativesPattern.MatchString(unwrap(strings.TrimSpace(line))) {
			return fail("multiple_answers")
		}
		// Standalone quantities/answers are claims, not arbitrary digits in
		// the reasoning. This also catches competing unlabeled final lines.
		if value, short := parseShortLine(line); short && value != finalAnswer {
			return fail("conflicting_answers")
		}
		for _, match := range declarationPattern.FindAllStringSubmatchIndex(line, -1) {
			value, valid := quantityValue(line[match[2]:match[3]])
			if !valid || value != finalAnswer {
				return fail("conflicting_answers")
			}
			if alternativePattern.MatchString(line[match[1]:]) || numericContinuationPattern.MatchString(line[match[1]:]) {
				return fail("multiple_answers")
			}
		}
	}
	if finalAnswer != normalizedExpected {
		return Grade{Status: StatusAnswerFail, Answer: finalAnswer, Reason: "answer_mismatch"}
	}
	return Grade{Status: StatusPass, Answer: finalAnswer, Reason: "match"}
}

func Evaluate(answer, expected string) bool {
	return ParseGrade(answer, expected).Status == StatusPass
}

func parseShortLine(line string) (string, bool) {
	line = unwrap(strings.TrimSpace(line))
	line = shortPrefix.ReplaceAllString(line, "")
	line = unwrap(strings.TrimSpace(line))
	match := quantityPattern.FindStringSubmatch(line)
	if match == nil {
		return "", false
	}
	return quantityValue(match[1])
}

// Only balanced wrappers around the whole short answer are removed. Removing
// formatting or spaces inside digits would incorrectly turn 2**1 or 2 1 into 21.
func unwrap(value string) string {
	for _, pair := range [][2]string{{"**", "**"}, {"__", "__"}, {"`", "`"}, {"\"", "\""}, {"'", "'"}, {"“", "”"}} {
		if strings.HasPrefix(value, pair[0]) && strings.HasSuffix(value, pair[1]) && len(value) > len(pair[0])+len(pair[1]) {
			return strings.TrimSpace(value[len(pair[0]) : len(value)-len(pair[1])])
		}
	}
	return value
}

func quantityValue(value string) (string, bool) {
	if value == "二十一" {
		return "21", true
	}
	return decimal(value)
}

func decimal(value string) (string, bool) {
	if !integerPattern.MatchString(value) {
		return "", false
	}
	n, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return "", false
	}
	return strconv.FormatUint(n, 10), true
}

// Normalize only fullwidth decimal digits, fullwidth colon, and line endings.
// Do not collapse interior spaces, apply broad compatibility normalization,
// or silently remove invisible characters; each could merge distinct tokens.
func normalize(value string) (string, bool) {
	if !utf8.ValidString(value) {
		return "", false
	}
	for _, r := range value {
		if unicode.Is(unicode.Cf, r) || (unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t') {
			return "", false
		}
	}
	value = strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n")
	return strings.Map(func(r rune) rune {
		switch {
		case r >= '０' && r <= '９':
			return '0' + r - '０'
		case r == '：':
			return ':'
		default:
			return r
		}
	}, value), true
}
