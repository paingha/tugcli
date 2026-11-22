package intelligence

import "regexp"

// instructionPatterns find text in the evidence that reads like
// instructions to a model: an application's log line, an event message or a
// custom resource's status written to steer whoever reads it. Such text is
// still sent, as the data it is, but its item is marked, and the guidance
// tells the model to ignore it and say it was there.
var instructionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:ignore|disregard|forget|override)\b.{0,40}\b(?:instructions?|prompts?|rules|guidance|above|previous|system)\b`),
	regexp.MustCompile(`(?i)\byou (?:are|must|should|will) (?:now )?(?:an? |the )?(?:ai|assistant|model|llm|claude|chatgpt|gpt)\b`),
	regexp.MustCompile(`(?i)\b(?:system prompt|new instructions|developer message|jailbreak)\b`),
	regexp.MustCompile(`(?i)\b(?:as|to) the (?:ai|assistant|model|llm)\b`),
	regexp.MustCompile(`(?i)\b(?:tell|ask|instruct) the (?:user|engineer|operator) to\b`),
	regexp.MustCompile(`(?i)\b(?:run|execute)\b.{0,20}\b(?:kubectl (?:delete|apply|exec|scale|patch|drain|replace)|rm -rf|curl\b.*\|\s*(?:ba)?sh)\b`),
}

// readsLikeInstructions reports whether text reads like instructions to a
// model.
func readsLikeInstructions(text string) bool {
	for _, re := range instructionPatterns {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}
