package evidence

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

func Canonicalize(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("canonicalize: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("canonicalize decode: %w", err)
	}
	if decoder.More() {
		return nil, fmt.Errorf("canonicalize: trailing JSON value")
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, decoded); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, value any) error {
	switch v := value.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if v {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		writeCanonicalString(buf, v)
	case json.Number:
		number, err := canonicalNumber(v.String())
		if err != nil {
			return err
		}
		buf.WriteString(number)
	case []any:
		buf.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonicalString(buf, key)
			buf.WriteByte(':')
			if err := writeCanonical(buf, v[key]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("canonicalize: unsupported type %T", value)
	}
	return nil
}

func writeCanonicalString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			if r < 0x20 {
				buf.WriteString(fmt.Sprintf(`\u%04x`, r))
			} else {
				buf.WriteRune(r)
			}
		}
	}
	buf.WriteByte('"')
}

func canonicalNumber(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("canonicalize: empty number")
	}
	if !strings.ContainsAny(raw, ".eE") {
		if raw[0] == '+' || (raw[0] == '0' && len(raw) > 1) || (raw[0] == '-' && len(raw) > 1 && raw[1] == '0') {
			return "", fmt.Errorf("canonicalize: invalid number %q", raw)
		}
		return raw, nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return "", fmt.Errorf("canonicalize: invalid number %q: %w", raw, err)
	}
	if value == 0 {
		return "0", nil
	}
	formatted := strconv.FormatFloat(value, 'g', -1, 64)
	formatted = strings.Replace(formatted, "E", "e", 1)
	if idx := strings.IndexByte(formatted, 'e'); idx >= 0 {
		mantissa := formatted[:idx]
		exponent := formatted[idx+1:]
		sign := ""
		if strings.HasPrefix(exponent, "+") {
			exponent = exponent[1:]
		} else if strings.HasPrefix(exponent, "-") {
			sign = "-"
			exponent = exponent[1:]
		}
		exponent = strings.TrimLeft(exponent, "0")
		if exponent == "" {
			exponent = "0"
		}
		formatted = mantissa + "e" + sign + exponent
	}
	return formatted, nil
}

func CanonicalDigest(v any) (string, error) {
	data, err := Canonicalize(v)
	if err != nil {
		return "", err
	}
	return DigestBytes(data), nil
}

func DigestBytes(data []byte) string {
	sum := sha256Sum(data)
	return "sha256:" + sum
}

func ReadAllCanonical(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	return Canonicalize(decoded)
}

func decodeCanonical(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(value)
}
