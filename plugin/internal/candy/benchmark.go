// Package candy defines the fixed Sub2API candy question and grades model
// answers without making network requests or examining token usage.
package candy

const (
	ID                    = "candy"
	Version               = "candy-sub2api-v1"
	Name                  = "糖果题（Sub2API）"
	DefaultExpectedAnswer = "21"
	Instructions          = "只输出最终答案数字，不要解释。"
	SourceURL             = "https://github.com/haowang02/codex-candy-eval"
)

// DefaultPrompt is kept identical to CandyPrompt in the user's local Sub2API
// backend/internal/service/pelican_scheduled.go. Do not add the expected answer
// or the explanation below to a model request. Changes require a new Version.
const DefaultPrompt = `在一个黑色的袋子里放有三种口味的糖果，每种糖果有两种不同的形状（圆形和五角星形，不同的形状靠手感可以分辨）。现已知不同口味的糖和不同形状的数量统计如下表。参赛者需要在活动前决定摸出的糖果数目，那么，最少取出多少个糖果才能保证手中同时拥有不同形状的苹果味和桃子味的糖？（同时手中有圆形苹果味匹配五角星桃子味糖果，或者有圆形桃子味匹配五角星苹果味糖果都满足要求）
苹果味 桃子味 西瓜味
圆形 7 9 8
五角星形 7 6 4`

// Explanation documents the interpretation underlying the reference answer.
// It is metadata for reviewers, never part of the question sent to a model.
const Explanation = "标准答案 21 依赖于可以通过触感选择形状、不能识别口味且不放回的解释。取 9 颗圆形及 12 颗星形即可保证满足要求。没有匹配时，圆形/星形数量最多落在 (24,4)、(8,17)、(15,11)、(17,10) 四种上界之一，(9,12) 不属于其中任何一种。总数不超过 20 的每一种形状分配都仍有失败构造。若不允许选择形状而只能无差别取出，28 颗仍可能失败，此时应为 29；默认题面沿用 Sub2API，保留这一隐含前提。单道固定题只能提供该次样本结果，不能证明模型整体能力或身份。"

// Benchmark is immutable-by-copy metadata. Only Prompt and Instructions belong
// in the upstream request; ExpectedAnswer and Explanation are local metadata.
type Benchmark struct {
	ID             string `json:"id"`
	Version        string `json:"version"`
	Name           string `json:"name"`
	Prompt         string `json:"prompt"`
	Instructions   string `json:"instructions"`
	ExpectedAnswer string `json:"expected_answer"`
	Explanation    string `json:"explanation"`
	SourceURL      string `json:"source_url"`
}

func Default() Benchmark {
	return Benchmark{
		ID: ID, Version: Version, Name: Name, Prompt: DefaultPrompt,
		Instructions: Instructions, ExpectedAnswer: DefaultExpectedAnswer,
		Explanation: Explanation, SourceURL: SourceURL,
	}
}
