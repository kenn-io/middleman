package github

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/rhysd/actionlint"
	"go.kenn.io/forge/internal/platform"
)

const MaxWorkflowDefinitionBytes = 1 << 20

// ParseManualWorkflow reads a GitHub Actions workflow file and reports whether
// it declares a workflow_dispatch trigger. actionlint owns the workflow file
// grammar; this function only projects its dispatch inputs onto the
// provider-neutral definition and converts defaults to their declared type.
func ParseManualWorkflow(
	name string,
	path string,
	webURL string,
	definitionSHA string,
	content []byte,
) (platform.WorkflowDefinition, bool, error) {
	definition := platform.WorkflowDefinition{
		ID:            path,
		Name:          name,
		Path:          path,
		WebURL:        webURL,
		DefinitionSHA: definitionSHA,
		Available:     true,
	}
	if len(content) > MaxWorkflowDefinitionBytes {
		return definition, false, fmt.Errorf(
			"workflow definition exceeds %d-byte limit",
			MaxWorkflowDefinitionBytes,
		)
	}

	workflow, parseErrors := actionlint.Parse(content)
	if len(parseErrors) > 0 {
		return definition, false, fmt.Errorf("parse workflow definition: %w", joinParseErrors(parseErrors))
	}
	if workflow == nil {
		return definition, false, errors.New("parse workflow definition: empty document")
	}

	var dispatch *actionlint.WorkflowDispatchEvent
	for _, event := range workflow.On {
		if candidate, ok := event.(*actionlint.WorkflowDispatchEvent); ok {
			dispatch = candidate
			break
		}
	}
	if dispatch == nil {
		return definition, false, nil
	}

	inputs, err := convertDispatchInputs(dispatch.Inputs)
	if err != nil {
		return definition, false, err
	}
	definition.Inputs = inputs
	return definition, true, nil
}

func joinParseErrors(parseErrors []*actionlint.Error) error {
	messages := make([]string, 0, len(parseErrors))
	for _, parseError := range parseErrors {
		messages = append(messages, fmt.Sprintf("line %d: %s", parseError.Line, parseError.Message))
	}
	return errors.New(strings.Join(messages, "; "))
}

// convertDispatchInputs returns inputs in file declaration order. actionlint
// lowercases the map keys, so the declared name is read from each input.
func convertDispatchInputs(inputs map[string]*actionlint.DispatchInput) ([]platform.WorkflowInput, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	ordered := make([]*actionlint.DispatchInput, 0, len(inputs))
	for _, input := range inputs {
		ordered = append(ordered, input)
	}
	slices.SortFunc(ordered, func(a, b *actionlint.DispatchInput) int {
		if a.Name.Pos.Line != b.Name.Pos.Line {
			return a.Name.Pos.Line - b.Name.Pos.Line
		}
		return a.Name.Pos.Col - b.Name.Pos.Col
	})

	converted := make([]platform.WorkflowInput, 0, len(ordered))
	for _, input := range ordered {
		value, err := convertDispatchInput(input)
		if err != nil {
			return nil, fmt.Errorf("parse workflow input %q: %w", input.Name.Value, err)
		}
		converted = append(converted, value)
	}
	return converted, nil
}

func convertDispatchInput(input *actionlint.DispatchInput) (platform.WorkflowInput, error) {
	converted := platform.WorkflowInput{Name: input.Name.Value, Type: inputType(input.Type)}
	if input.Description != nil {
		converted.Description = input.Description.Value
	}
	if input.Required != nil {
		if input.Required.Expression != nil {
			return converted, errors.New("required must be a literal boolean")
		}
		converted.Required = input.Required.Value
	}
	for _, option := range input.Options {
		if slices.Contains(converted.Options, option.Value) {
			return converted, fmt.Errorf("duplicate choice option %q", option.Value)
		}
		converted.Options = append(converted.Options, option.Value)
	}
	if converted.Type == platform.WorkflowInputChoice && len(converted.Options) == 0 {
		return converted, errors.New("choice input requires options")
	}
	if converted.Type != platform.WorkflowInputChoice && len(converted.Options) > 0 {
		return converted, errors.New("options are only valid for choice inputs")
	}
	if input.Default == nil {
		return converted, nil
	}

	value, err := typedDefault(input.Default.Value, converted.Type)
	if err != nil {
		return converted, err
	}
	if converted.Type == platform.WorkflowInputChoice && !slices.Contains(converted.Options, value.(string)) {
		return converted, fmt.Errorf("default %q is not one of the choice options", value)
	}
	converted.Default = value
	converted.HasDefault = true
	return converted, nil
}

func inputType(kind actionlint.WorkflowDispatchEventInputType) platform.WorkflowInputType {
	switch kind {
	case actionlint.WorkflowDispatchEventInputTypeNumber:
		return platform.WorkflowInputNumber
	case actionlint.WorkflowDispatchEventInputTypeBoolean:
		return platform.WorkflowInputBoolean
	case actionlint.WorkflowDispatchEventInputTypeChoice:
		return platform.WorkflowInputChoice
	case actionlint.WorkflowDispatchEventInputTypeEnvironment:
		return platform.WorkflowInputEnvironment
	default:
		return platform.WorkflowInputString
	}
}

// typedDefault converts the raw YAML scalar text into the value shape the
// dispatch form and validation expect for the declared input type.
func typedDefault(raw string, kind platform.WorkflowInputType) (any, error) {
	switch kind {
	case platform.WorkflowInputBoolean:
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("default does not match type %q", kind)
		}
		return value, nil
	case platform.WorkflowInputNumber:
		if value, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return int(value), nil
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
			return nil, fmt.Errorf("default does not match type %q", kind)
		}
		return value, nil
	default:
		return raw, nil
	}
}
