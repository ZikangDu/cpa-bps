package candy

import (
	"strings"
	"testing"
)

// Enumerate every feasible six-category draw independently of the prose proof.
// This verifies both the 21 minimum with shape selection and the 29 threshold
// when the participant cannot choose shapes.
func TestReferenceAnswerByExhaustiveDrawEnumeration(t *testing.T) {
	var failing [25][18]bool
	maxFailingTotal := 0
	for circleApple := 0; circleApple <= 7; circleApple++ {
		for circlePeach := 0; circlePeach <= 9; circlePeach++ {
			for circleWatermelon := 0; circleWatermelon <= 8; circleWatermelon++ {
				for starApple := 0; starApple <= 7; starApple++ {
					for starPeach := 0; starPeach <= 6; starPeach++ {
						for starWatermelon := 0; starWatermelon <= 4; starWatermelon++ {
							if circleApple*starPeach > 0 || circlePeach*starApple > 0 {
								continue
							}
							circle := circleApple + circlePeach + circleWatermelon
							star := starApple + starPeach + starWatermelon
							failing[circle][star] = true
							maxFailingTotal = max(maxFailingTotal, circle+star)
						}
					}
				}
			}
		}
	}
	minGuaranteed := 42
	for circle := 0; circle <= 24; circle++ {
		for star := 0; star <= 17; star++ {
			if !failing[circle][star] {
				minGuaranteed = min(minGuaranteed, circle+star)
			}
			// Independent cross-check of the four extremal failure regions.
			inFailureRegion := (circle <= 24 && star <= 4) ||
				(circle <= 8 && star <= 17) ||
				(circle <= 15 && star <= 11) ||
				(circle <= 17 && star <= 10)
			if failing[circle][star] != inFailureRegion {
				t.Fatalf("failure-region proof disagrees with enumeration at (%d,%d)", circle, star)
			}
		}
	}
	if minGuaranteed != 21 || failing[9][12] {
		t.Fatalf("shape-selection minimum = %d; (9,12) can fail = %v", minGuaranteed, failing[9][12])
	}
	if maxFailingTotal+1 != 29 {
		t.Fatalf("uncontrolled-draw threshold = %d, want 29", maxFailingTotal+1)
	}
}

func TestDefaultKeepsExpectedAnswerOutOfModelInput(t *testing.T) {
	b := Default()
	if b.ExpectedAnswer != "21" || b.Version == "" || b.SourceURL == "" {
		t.Fatalf("incomplete benchmark: %#v", b)
	}
	if strings.Contains(b.Prompt+b.Instructions, b.ExpectedAnswer) || strings.Contains(b.Prompt, "9 颗圆形") {
		t.Fatal("model input must not include the reference answer or its strategy")
	}
	b.Prompt = "changed by a caller"
	if Default().Prompt != DefaultPrompt {
		t.Fatal("callers must not mutate the shared benchmark")
	}
}
