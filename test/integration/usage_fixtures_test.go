package integration

import "net/http"

// jsonUsageResponse returns a successful deterministic OpenAI response.
func jsonUsageResponse(input int, output int) StubResponse {
	return jsonUsageResponseForModel("upstream-model", input, output)
}

// jsonUsageResponseForModel returns a successful response for one resolved model.
func jsonUsageResponseForModel(model string, input int, output int) StubResponse {
	return StubResponse{
		Status:  http.StatusOK,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body: `{
			"id":"chatcmpl-usage",
			"object":"chat.completion",
			"created":1,
			"model":"` + model + `",
			"choices":[{
				"index":0,
				"message":{"role":"assistant","content":"fixture-response"},
				"finish_reason":"stop"
			}],
			"usage":{
				"prompt_tokens":` + integerString(input) + `,
				"completion_tokens":` + integerString(output) + `,
				"total_tokens":` + integerString(input+output) + `
			}
		}`,
	}
}

// streamingUsageResponse returns an SSE stream with final usage.
func streamingUsageResponse(input int, output int) StubResponse {
	return StubResponse{
		Status:  http.StatusOK,
		Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: `data: {"id":"chatcmpl-stream","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[{"index":0,"delta":{"role":"assistant","content":"fixture-response"},"finish_reason":null}]}

data: {"id":"chatcmpl-stream","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":` + integerString(input) + `,"completion_tokens":` + integerString(output) + `,"total_tokens":` + integerString(input+output) + `}}

data: [DONE]

`,
	}
}

// integerString renders a small non-negative fixture integer without formatting dependencies.
func integerString(value int) string {
	const digits = "0123456789"
	if value == 0 {
		return "0"
	}
	result := make([]byte, 0, 8)
	for value > 0 {
		result = append(result, digits[value%10])
		value /= 10
	}
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return string(result)
}
