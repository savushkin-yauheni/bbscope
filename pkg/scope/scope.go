package scope

import (
	"encoding/json"
	"fmt"
	"strings"
)

type ScopeElement struct {
	Target      string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category"`
}

type ProgramData struct {
	Url        string         `json:"url"`
	Name       string         `json:"name"`
	InScope    []ScopeElement `json:"assets"`
	OutOfScope []ScopeElement `json:"-"`
}

func PrintProgramScope(programScope []ProgramData, outputFlags string, delimiter string) {
	lines := ""
	jsonData, _ := json.Marshal(programScope)
	lines += string(jsonData)
	lines = strings.TrimSuffix(lines, "\n")

	if len(lines) > 0 {
		fmt.Println(lines)
	}
}
