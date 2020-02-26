// SPDX-License-Identifier: Unlicense OR MIT

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/format"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"gioui.org/gpu/backend"
)

// This program generates shader variants for
// multiple GPU backends (OpenGL ES, Direct3D 11...)
// from a single source.

var packageName = flag.String("package", "", "specify Go package name")

type shaderArgs struct {
	FetchColorExpr string
	Header         string
}

func main() {
	flag.Parse()
	if err := generate(); err != nil {
		fmt.Fprintf(os.Stderr, "generate: %v\n", err)
		os.Exit(1)
	}
}

func generate() error {
	tmp, err := ioutil.TempDir("", "shader-convert")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	glslcc, err := exec.LookPath("glslcc")
	if err != nil {
		return err
	}
	fxc, err := exec.LookPath("fxc")
	fxcFound := err == nil
	shaders, err := filepath.Glob("shaders/*")
	if err != nil {
		return err
	}
	var out bytes.Buffer
	out.WriteString("// Code generated by build.go. DO NOT EDIT.\n\n")
	fmt.Fprintf(&out, "package %s\n\n", *packageName)
	fmt.Fprintf(&out, "import %q\n\n", "gioui.org/gpu/backend")

	out.WriteString("var (\n")

	for _, shader := range shaders {
		const nvariants = 2
		var variants [nvariants]struct {
			backend.ShaderSources
			hlslSrc string
		}
		args := [nvariants]shaderArgs{
			{
				FetchColorExpr: `_color`,
				Header:         `layout(binding=0) uniform Color { vec4 _color; };`,
			},
			{
				FetchColorExpr: `texture(tex, vUV)`,
				Header:         `layout(binding=0) uniform sampler2D tex;`,
			},
		}
		for i := range args {
			glsl100, reflect, err := convertShader(tmp, glslcc, shader, "gles", "100", &args[i], false)
			if err != nil {
				return err
			}
			// Make the GL ES 2 source compatible with desktop GL 3.
			glsl100 = "#version 100\n" + glsl100
			if err := parseReflection(reflect, &variants[i].ShaderSources); err != nil {
				return err
			}
			glsl300, _, err := convertShader(tmp, glslcc, shader, "gles", "300", &args[i], false)
			if err != nil {
				return err
			}
			hlsl, _, err := convertShader(tmp, glslcc, shader, "hlsl", "40", &args[i], false)
			if err != nil {
				return err
			}
			var hlslProf string
			switch filepath.Ext(shader) {
			case ".frag":
				hlslProf = "ps"
			case ".vert":
				hlslProf = "vs"
			default:
				return fmt.Errorf("unrecognized shader type %s", shader)
			}
			var hlslc []byte
			if fxcFound {
				hlslc, err = compileHLSL(tmp, fxc, hlsl, "main", hlslProf+"_4_0")
				if err != nil {
					return err
				}
			}
			variants[i].GLSL100ES = glsl100
			variants[i].GLSL300ES = glsl300
			variants[i].hlslSrc = hlsl
			variants[i].HLSL = hlslc
		}
		name := filepath.Base(shader)
		name = strings.ReplaceAll(name, ".", "_")
		fmt.Fprintf(&out, "\tshader_%s = ", name)
		// If the shader don't use the variant arguments, output
		// only a single version.
		multiVariant := variants[0].GLSL100ES != variants[1].GLSL100ES
		if multiVariant {
			fmt.Fprintf(&out, "[...]backend.ShaderSources{\n")
		}
		for _, src := range variants {
			fmt.Fprintf(&out, "backend.ShaderSources{\n")
			if len(src.Inputs) > 0 {
				fmt.Fprintf(&out, "Inputs: %#v,\n", src.Inputs)
			}
			if u := src.Uniforms; len(u.Blocks) > 0 {
				fmt.Fprintf(&out, "Uniforms: backend.UniformsReflection{\n")
				fmt.Fprintf(&out, "Blocks: %#v,\n", u.Blocks)
				fmt.Fprintf(&out, "Locations: %#v,\n", u.Locations)
				fmt.Fprintf(&out, "Size: %d,\n", u.Size)
				fmt.Fprintf(&out, "},\n")
			}
			if len(src.Textures) > 0 {
				fmt.Fprintf(&out, "Textures: %#v,\n", src.Textures)
			}
			fmt.Fprintf(&out, "GLSL100ES: %#v,\n", src.GLSL100ES)
			fmt.Fprintf(&out, "GLSL300ES: %#v,\n", src.GLSL300ES)
			fmt.Fprintf(&out, "/*\n%s\n*/\n", src.hlslSrc)
			fmt.Fprintf(&out, "HLSL: %#v,\n", src.HLSL)
			fmt.Fprintf(&out, "}")
			if multiVariant {
				fmt.Fprintf(&out, ",")
			}
			fmt.Fprintf(&out, "\n")
			if !multiVariant {
				break
			}
		}
		if multiVariant {
			fmt.Fprintf(&out, "}\n")
		}
	}
	out.WriteString(")")
	gosrc, err := format.Source(out.Bytes())
	if err != nil {
		return fmt.Errorf("shader.go: %v", err)
	}
	return ioutil.WriteFile("shaders.go", gosrc, 0644)
}

func parseReflection(jsonData []byte, info *backend.ShaderSources) error {
	type InputReflection struct {
		ID            int    `json:"id"`
		Name          string `json:"name"`
		Location      int    `json:"location"`
		Semantic      string `json:"semantic"`
		SemanticIndex int    `json:"semantic_index"`
		Type          string `json:"type"`
	}
	type UniformMemberReflection struct {
		Name   string `json:"name"`
		Type   string `json:"type"`
		Offset int    `json:"offset"`
		Size   int    `json:"size"`
	}
	type UniformBufferReflection struct {
		ID      int                       `json:"id"`
		Name    string                    `json:"name"`
		Set     int                       `json:"set"`
		Binding int                       `json:"binding"`
		Size    int                       `json:"block_size"`
		Members []UniformMemberReflection `json:"members"`
	}
	type TextureReflection struct {
		ID        int    `json:"id"`
		Name      string `json:"name"`
		Set       int    `json:"set"`
		Binding   int    `json:"binding"`
		Dimension string `json:"dimension"`
		Format    string `json:"format"`
	}
	type shaderReflection struct {
		Inputs         []InputReflection         `json:"inputs"`
		UniformBuffers []UniformBufferReflection `json:"uniform_buffers"`
		Textures       []TextureReflection       `json:"textures"`
	}
	type shaderMetadata struct {
		VS shaderReflection `json:"vs"`
		FS shaderReflection `json:"fs"`
	}
	var reflect shaderMetadata
	if err := json.Unmarshal(jsonData, &reflect); err != nil {
		return fmt.Errorf("parseReflection: %v", err)
	}
	inputRef := reflect.VS.Inputs
	for _, input := range inputRef {
		dataType, dataSize, err := parseDataType(input.Type)
		if err != nil {
			return fmt.Errorf("parseReflection: %v", err)
		}
		info.Inputs = append(info.Inputs, backend.InputLocation{
			Name:          input.Name,
			Location:      input.Location,
			Semantic:      input.Semantic,
			SemanticIndex: input.SemanticIndex,
			Type:          dataType,
			Size:          dataSize,
		})
	}
	sort.Slice(info.Inputs, func(i, j int) bool {
		return info.Inputs[i].Location < info.Inputs[j].Location
	})
	shaderBlocks := reflect.VS.UniformBuffers
	if len(shaderBlocks) == 0 {
		shaderBlocks = reflect.FS.UniformBuffers
	}
	blockOffset := 0
	for _, block := range shaderBlocks {
		info.Uniforms.Blocks = append(info.Uniforms.Blocks, backend.UniformBlock{
			Name:    block.Name,
			Binding: block.Binding,
		})
		for _, member := range block.Members {
			dataType, size, err := parseDataType(member.Type)
			if err != nil {
				return fmt.Errorf("parseReflection: %v", err)
			}
			info.Uniforms.Locations = append(info.Uniforms.Locations, backend.UniformLocation{
				// Synthetic name generated by glslcc.
				Name:   fmt.Sprintf("_%d.%s", block.ID, member.Name),
				Type:   dataType,
				Size:   size,
				Offset: blockOffset + member.Offset,
			})
		}
		blockOffset += block.Size
	}
	info.Uniforms.Size = blockOffset
	textures := reflect.VS.Textures
	if len(textures) == 0 {
		textures = reflect.FS.Textures
	}
	for _, texture := range textures {
		info.Textures = append(info.Textures, backend.TextureBinding{
			Name:    texture.Name,
			Binding: texture.Binding,
		})
	}
	return nil
}

func parseDataType(t string) (backend.DataType, int, error) {
	switch t {
	case "float":
		return backend.DataTypeFloat, 1, nil
	case "float2":
		return backend.DataTypeFloat, 2, nil
	case "float3":
		return backend.DataTypeFloat, 3, nil
	case "float4":
		return backend.DataTypeFloat, 4, nil
	case "int":
		return backend.DataTypeInt, 1, nil
	case "int2":
		return backend.DataTypeInt, 2, nil
	case "int3":
		return backend.DataTypeInt, 3, nil
	case "int4":
		return backend.DataTypeInt, 4, nil
	default:
		return 0, 0, fmt.Errorf("unsupported input data type: %s", t)
	}
}

func compileHLSL(tmp, fxc, src, entry, profile string) ([]byte, error) {
	tmpfile := filepath.Join(tmp, "shader.hlsl")
	if err := ioutil.WriteFile(tmpfile, []byte(src), 0644); err != nil {
		return nil, err
	}
	outFile := filepath.Join(tmp, "shader.bin")
	cmd := exec.Command(fxc,
		"/T", profile,
		"/E", entry,
		"/nologo",
		"/Fo", outFile,
		tmpfile,
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return ioutil.ReadFile(outFile)
}

func convertShader(tmp, glslcc, path, lang, profile string, args *shaderArgs, flattenUBOs bool) (string, []byte, error) {
	shaderTmpl, err := template.ParseFiles(path)
	if err != nil {
		return "", nil, err
	}
	var buf bytes.Buffer
	if err := shaderTmpl.Execute(&buf, args); err != nil {
		return "", nil, err
	}
	tmppath := filepath.Join(tmp, filepath.Base(path))
	if err := ioutil.WriteFile(tmppath, buf.Bytes(), 0644); err != nil {
		return "", nil, err
	}
	defer os.Remove(tmppath)
	var progFlag string
	var progSuffix string
	switch filepath.Ext(path) {
	case ".vert":
		progFlag = "--vert"
		progSuffix = "vs"
	case ".frag":
		progFlag = "--frag"
		progSuffix = "fs"
	default:
		return "", nil, fmt.Errorf("unrecognized shader type: %s", path)
	}
	cmd := exec.Command(glslcc,
		"--silent",
		"--optimize",
		"--reflect",
		"--output", filepath.Join(tmp, "shader"),
		"--lang", lang,
		"--profile", profile,
		progFlag, tmppath,
	)
	if flattenUBOs {
		cmd.Args = append(cmd.Args, "--flatten-ubos")
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", nil, fmt.Errorf("%s: %v", path, err)
	}
	f, err := os.Open(filepath.Join(tmp, "shader_"+progSuffix))
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	defer os.Remove(f.Name())
	src, err := ioutil.ReadAll(f)
	if err != nil {
		return "", nil, err
	}
	reflect, err := ioutil.ReadFile(filepath.Join(tmp, "shader_"+progSuffix+".json"))
	if err != nil {
		return "", nil, err
	}
	return string(src), reflect, nil
}
