package canon

import (
	"encoding/json"
	"testing"
)

// Vectors from RFC 8785 sections 3.2.2 and 3.2.3. A JSON library's sorted-keys
// mode does not produce these, which is why this package exists.
func TestRFC8785Vectors(t *testing.T) {
	cases := []struct{ in, want string }{
		{
			in:   `{"numbers": [333333333.33333329, 1E30, 4.50, 2e-3, 0.000000000000000000000000001], "string": "\u20ac$\u000F\u000aA'\u0042\u0022\u005c\\\"\/", "literals": [null, true, false]}`,
			want: `{"literals":[null,true,false],"numbers":[333333333.3333333,1e+30,4.5,0.002,1e-27],"string":"€$\u000f\nA'B\"\\\\\"/"}`,
		},
		{
			in:   `{"\u20ac": "Euro Sign", "\r": "Carriage Return", "\ufb33": "Hebrew Letter Dalet With Dagesh", "1": "One", "\ud83d\ude00": "Emoji: Grinning Face", "\u0080": "Control", "\u00f6": "Latin Small Letter O With Diaeresis"}`,
			want: "{\"\\r\":\"Carriage Return\",\"1\":\"One\",\"\u0080\":\"Control\",\"\u00f6\":\"Latin Small Letter O With Diaeresis\",\"\u20ac\":\"Euro Sign\",\"\U0001F600\":\"Emoji: Grinning Face\",\"\ufb33\":\"Hebrew Letter Dalet With Dagesh\"}",
		},
		{in: `[-0, 100.0, 1e-7, 1e21, "<&>"]`, want: `[0,100,1e-7,1e+21,"<&>"]`},
	}
	for _, c := range cases {
		var v any
		if err := json.Unmarshal([]byte(c.in), &v); err != nil {
			t.Fatal(err)
		}
		got, err := Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != c.want {
			t.Errorf("\n got %s\nwant %s", got, c.want)
		}
	}
}

func TestStructFieldOrderDoesNotMatter(t *testing.T) {
	type a struct {
		Z int `json:"z"`
		A int `json:"a"`
	}
	type b struct {
		A int `json:"a"`
		Z int `json:"z"`
	}
	ha, _ := SHA256(a{1, 2})
	hb, _ := SHA256(b{2, 1})
	if ha != hb {
		t.Error("same object, different hash")
	}
}
