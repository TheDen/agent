package agent

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/buildkite/agent/env"
	"github.com/buildkite/agent/yamltojson"
	"github.com/buildkite/interpolate"

	// This is a fork of gopkg.in/yaml.v2 that fixes anchors with MapSlice
	yaml "github.com/buildkite/yaml"
)

type PipelineParser struct {
	Env             *env.Environment
	Filename        string
	Pipeline        []byte
	NoInterpolation bool
}

func (p PipelineParser) Parse() (interface{}, error) {
	if p.Env == nil {
		p.Env = env.FromSlice(os.Environ())
	}

	var errPrefix string
	if p.Filename == "" {
		errPrefix = "Failed to parse pipeline"
	} else {
		errPrefix = fmt.Sprintf("Failed to parse %s", p.Filename)
	}

	// If interpolation is disabled, just parse and return
	if p.NoInterpolation {
		var result interface{}
		if err := yamltojson.UnmarshalAsStringMap([]byte(p.Pipeline), &result); err != nil {
			return nil, fmt.Errorf("%s: %v", errPrefix, formatYAMLError(err))
		}
		return result, nil
	}

	var pipeline interface{}
	var pipelineAsSlice []interface{}

	// Historically we support uploading just steps, so we parse it as either a
	// slice, or if it's a map we need to do environment block processing
	if err := yaml.Unmarshal([]byte(p.Pipeline), &pipelineAsSlice); err == nil {
		pipeline = pipelineAsSlice
	} else {
		pipelineAsMap, err := p.parseWithEnv()
		if err != nil {
			return nil, fmt.Errorf("%s: %v", errPrefix, formatYAMLError(err))
		}
		pipeline = pipelineAsMap
	}

	// Recursively go through the entire pipeline and perform environment
	// variable interpolation on strings
	interpolated, err := p.interpolate(pipeline)
	if err != nil {
		return nil, err
	}

	// Now we roundtrip this back into YAML bytes and back into a generic interface{}
	// that works with all upstream code (which likes working with JSON). Specifically we
	// need to convert the map[interface{}]interface{}'s that YAML likes into JSON compatible
	// map[string]interface{}
	b, err := yaml.Marshal(interpolated)
	if err != nil {
		return nil, err
	}

	var result interface{}
	if err := yamltojson.UnmarshalAsStringMap(b, &result); err != nil {
		return nil, fmt.Errorf("%s: %v", errPrefix, formatYAMLError(err))
	}

	return result, nil
}

func (p PipelineParser) parseWithEnv() (interface{}, error) {
	var pipeline yaml.MapSlice

	// Initially we unmarshal this into a yaml.MapSlice so that we preserve the order of maps
	if err := yaml.Unmarshal([]byte(p.Pipeline), &pipeline); err != nil {
		return nil, err
	}

	// Preprocess any env tat are defined in the top level block and place them into env for
	// later interpolation into env blocks
	if item, ok := mapSliceItem("env", pipeline); ok {
		if envMap, ok := item.Value.(yaml.MapSlice); ok {
			if err := p.interpolateEnvBlock(envMap); err != nil {
				return nil, err
			}
		} else {
			return nil, fmt.Errorf("Expected pipeline top-level env block to be a map, got %T", item)
		}
	}

	return pipeline, nil
}

func mapSliceItem(key string, s yaml.MapSlice) (yaml.MapItem, bool) {
	for _, item := range s {
		if k, ok := item.Key.(string); ok && k == key {
			return item, true
		}
	}
	return yaml.MapItem{}, false
}

func (p PipelineParser) interpolateEnvBlock(envMap yaml.MapSlice) error {
	for _, item := range envMap {
		k, ok := item.Key.(string)
		if !ok {
			return fmt.Errorf("Unexpected type of %T for env block key %v", item.Key, item.Key)
		}
		switch tv := item.Value.(type) {
		case string:
			interpolated, err := interpolate.Interpolate(p.Env, tv)
			if err != nil {
				return err
			}
			p.Env.Set(k, interpolated)
		}
	}
	return nil
}

func formatYAMLError(err error) error {
	return errors.New(strings.TrimPrefix(err.Error(), "yaml: "))
}

// interpolate function inspired from: https://gist.github.com/hvoecking/10772475

func (p PipelineParser) interpolate(obj interface{}) (interface{}, error) {
	// Make sure there's something actually to interpolate
	if obj == nil {
		return nil, nil
	}

	// Wrap the original in a reflect.Value
	original := reflect.ValueOf(obj)

	// Make a copy that we'll add the new values to
	copy := reflect.New(original.Type()).Elem()

	err := p.interpolateRecursive(copy, original)
	if err != nil {
		return nil, err
	}

	// Remove the reflection wrapper
	return copy.Interface(), nil
}

func (p PipelineParser) interpolateRecursive(copy, original reflect.Value) error {
	switch original.Kind() {
	// If it is a pointer we need to unwrap and call once again
	case reflect.Ptr:
		// To get the actual value of the original we have to call Elem()
		// At the same time this unwraps the pointer so we don't end up in
		// an infinite recursion
		originalValue := original.Elem()

		// Check if the pointer is nil
		if !originalValue.IsValid() {
			return nil
		}

		// Allocate a new object and set the pointer to it
		copy.Set(reflect.New(originalValue.Type()))

		// Unwrap the newly created pointer
		err := p.interpolateRecursive(copy.Elem(), originalValue)
		if err != nil {
			return err
		}

	// If it is an interface (which is very similar to a pointer), do basically the
	// same as for the pointer. Though a pointer is not the same as an interface so
	// note that we have to call Elem() after creating a new object because otherwise
	// we would end up with an actual pointer
	case reflect.Interface:
		// Get rid of the wrapping interface
		originalValue := original.Elem()

		// Check to make sure the interface isn't nil
		if !originalValue.IsValid() {
			return nil
		}

		// Create a new object. Now new gives us a pointer, but we want the value it
		// points to, so we have to call Elem() to unwrap it
		copyValue := reflect.New(originalValue.Type()).Elem()

		err := p.interpolateRecursive(copyValue, originalValue)
		if err != nil {
			return err
		}

		copy.Set(copyValue)

	// If it is a struct we interpolate each field
	case reflect.Struct:
		for i := 0; i < original.NumField(); i += 1 {
			err := p.interpolateRecursive(copy.Field(i), original.Field(i))
			if err != nil {
				return err
			}
		}

	// If it is a slice we create a new slice and interpolate each element
	case reflect.Slice:
		copy.Set(reflect.MakeSlice(original.Type(), original.Len(), original.Cap()))

		for i := 0; i < original.Len(); i += 1 {
			err := p.interpolateRecursive(copy.Index(i), original.Index(i))
			if err != nil {
				return err
			}
		}

	// If it is a map we create a new map and interpolate each value
	case reflect.Map:
		copy.Set(reflect.MakeMap(original.Type()))

		for _, key := range original.MapKeys() {
			originalValue := original.MapIndex(key)

			// New gives us a pointer, but again we want the value
			copyValue := reflect.New(originalValue.Type()).Elem()
			err := p.interpolateRecursive(copyValue, originalValue)
			if err != nil {
				return err
			}

			// Also interpolate the key if it's a string
			if key.Kind() == reflect.String {
				interpolatedKey, err := interpolate.Interpolate(p.Env, key.Interface().(string))
				if err != nil {
					return err
				}
				copy.SetMapIndex(reflect.ValueOf(interpolatedKey), copyValue)
			} else {
				copy.SetMapIndex(key, copyValue)
			}
		}

	// If it is a string interpolate it (yay finally we're doing what we came for)
	case reflect.String:
		interpolated, err := interpolate.Interpolate(p.Env, original.Interface().(string))
		if err != nil {
			return err
		}
		copy.SetString(interpolated)

	// And everything else will simply be taken from the original
	default:
		copy.Set(original)
	}

	return nil
}
