package rongcloud

import "encoding/json"

func encodeTextContent(text string) string {
	b, _ := json.Marshal(textContent{Content: text})
	return string(b)
}
