package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

const (
	ParseError     = -32700
	InvalidRequest = -32600
	MethodNotFound = -32601
	InvalidParams  = -32602
	InternalError  = -32603

	MCPServerNotInitialized = -32002
	MCPUnknownError         = -32001
)

type JSONRPCRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      int            `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params,omitempty"`
}

type JSONRPCResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Result  any    `json:"result"`
}

type JSONRPCError struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      int            `json:"id"`
	Error   map[string]any `json:"error"`
}

type JSONRPCNotification struct {
	JSONRPC string         `json:"jsonrpc"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params,omitempty"`
}

func SerializeMessage(msg any) string {
	pairs := []jsonPair{{Key: "jsonrpc", Value: "2.0"}}
	setJSONRPC := func(value string) {
		pairs[0].Value = firstNonEmptyString(value, "2.0")
	}
	switch m := msg.(type) {
	case JSONRPCRequest:
		setJSONRPC(m.JSONRPC)
		pairs = append(pairs, jsonPair{Key: "id", Value: m.ID}, jsonPair{Key: "method", Value: m.Method})
		if m.Params != nil {
			pairs = append(pairs, jsonPair{Key: "params", Value: m.Params})
		}
	case JSONRPCResponse:
		setJSONRPC(m.JSONRPC)
		pairs = append(pairs, jsonPair{Key: "id", Value: m.ID}, jsonPair{Key: "result", Value: m.Result})
	case JSONRPCError:
		setJSONRPC(m.JSONRPC)
		pairs = append(pairs, jsonPair{Key: "id", Value: m.ID}, jsonPair{Key: "error", Value: m.Error})
	case JSONRPCNotification:
		setJSONRPC(m.JSONRPC)
		pairs = append(pairs, jsonPair{Key: "method", Value: m.Method})
		if m.Params != nil {
			pairs = append(pairs, jsonPair{Key: "params", Value: m.Params})
		}
	case *JSONRPCRequest:
		return SerializeMessage(*m)
	case *JSONRPCResponse:
		return SerializeMessage(*m)
	case *JSONRPCError:
		return SerializeMessage(*m)
	case *JSONRPCNotification:
		return SerializeMessage(*m)
	}
	return marshalJSONPairs(pairs)
}

func ParseMessage(data string) any {
	var obj map[string]any
	if err := json.Unmarshal([]byte(data), &obj); err != nil {
		return nil
	}
	if obj["jsonrpc"] != "2.0" {
		return nil
	}
	_, hasID := obj["id"]
	_, hasMethod := obj["method"]
	_, hasResult := obj["result"]
	_, hasError := obj["error"]
	if hasMethod && !hasID {
		return JSONRPCNotification{JSONRPC: "2.0", Method: stringValue(obj["method"]), Params: mapValue(obj["params"])}
	}
	if hasMethod && hasID {
		return JSONRPCRequest{JSONRPC: "2.0", ID: intValue(obj["id"]), Method: stringValue(obj["method"]), Params: mapValue(obj["params"])}
	}
	if hasID && hasError {
		errMap := mapValue(obj["error"])
		if errMap == nil {
			return nil
		}
		return JSONRPCError{JSONRPC: "2.0", ID: intValue(obj["id"]), Error: map[string]any{
			"code":    intValue(errMap["code"]),
			"message": stringValue(errMap["message"]),
			"data":    errMap["data"],
		}}
	}
	if hasID && hasResult {
		return JSONRPCResponse{JSONRPC: "2.0", ID: intValue(obj["id"]), Result: obj["result"]}
	}
	return nil
}

func MakeRequest(method string, params map[string]any, requestID int) JSONRPCRequest {
	return JSONRPCRequest{JSONRPC: "2.0", ID: requestID, Method: method, Params: params}
}

func MakeNotification(method string, params map[string]any) JSONRPCNotification {
	return JSONRPCNotification{JSONRPC: "2.0", Method: method, Params: params}
}

func stringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func intValue(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	case string:
		parsed, err := strconv.Atoi(n)
		if err == nil {
			return parsed
		}
	case bool:
		if n {
			return 1
		}
	default:
		return 0
	}
	return 0
}

func mapValue(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func firstNonEmptyString(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

type jsonPair struct {
	Key   string
	Value any
}

func marshalJSONPairs(pairs []jsonPair) string {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, pair := range pairs {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(jsonNoEscape(pair.Key))
		buf.WriteByte(':')
		buf.WriteString(jsonNoEscape(pair.Value))
	}
	buf.WriteByte('}')
	return buf.String()
}

func jsonNoEscape(value any) string {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	return string(bytes.TrimRight(buf.Bytes(), "\n"))
}
