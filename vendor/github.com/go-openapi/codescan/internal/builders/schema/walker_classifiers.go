// SPDX-FileCopyrightText: Copyright 2015-2025 go-swagger maintainers
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"go/ast"
	"go/token"
	"go/types"
	"strconv"
	"strings"

	"github.com/go-openapi/codescan/internal/builders/resolvers"
	"github.com/go-openapi/codescan/internal/builders/validations"
	"github.com/go-openapi/codescan/internal/ifaces"
	"github.com/go-openapi/codescan/internal/parsers/grammar"
	"github.com/go-openapi/codescan/internal/scanner"
	"golang.org/x/tools/go/packages"
)

// findAnnotationArg returns the first positional argument of the first Block of the given
// annotation kind in cg, filtered to non-empty single-word arguments and read through the
// ParseBlocks cache.
//
// # Details
//
// See [§classifier-walkers](./README.md#classifier-walkers) — the single-word filter's rationale
// and the named-basic prose-trap fixture it protects against.
func (s *Builder) findAnnotationArg(cg *ast.CommentGroup, kind grammar.AnnotationKind) (string, bool) {
	for _, b := range s.ParseBlocks(cg) {
		if b.AnnotationKind() != kind {
			continue
		}
		arg, ok := b.AnnotationArg()
		if !ok {
			continue
		}
		if strings.ContainsAny(arg, " \t") {
			continue
		}
		return arg, true
	}
	return "", false
}

// Per-call-site classifier walkers.
//
// One walker per call site; each documents in its godoc which `swagger:<kind>` annotations it
// consumes.
//
// # Details
//
// See [§classifier-walkers](./README.md#classifier-walkers) — the design rationale, the walker
// inventory table, and the `findAnnotationArg` single-word filter contract that backs the
// kind-specific lookups.

// classifierTextMarshal is the named-type walker fired at the text-marshal short-circuit
// (`buildFromTextMarshal` end-of-pipe).
//
// Consumes only `swagger:strfmt`.
// On match writes `{string, <format>}` to tgt and returns true.
func (s *Builder) classifierTextMarshal(tpe types.Type, tgt ifaces.SwaggerTypable) (resolved bool) {
	decl, ok := s.Ctx.DeclForType(tpe)
	if !ok || decl == nil {
		return false
	}

	if name, ok := s.findAnnotationArg(decl.Comments, grammar.AnnStrfmt); ok {
		tgt.Typed("string", name)
		return true
	}

	return false
}

// classifierNamedTypeOverride is the named-type walker fired in `buildFromType`'s named-fallback
// and `buildFromStruct`'s pre- pass.
//
// Consumes only `swagger:type` (the explicit type-override annotation).
//
// It is the single resolution point for `swagger:type` on a named type: it routes the argument
// through resolveTypeOverride (always inlining — keyword scalars / Go builtins / `[]T` / `inline`
// / `array` / type-name refs), and applies a co-present `swagger:strfmt` as a supplementary format
// only when compatible with the resolved type (F3 — see .claude/plans/archive/quirks-F-series-fix.md).
// ownType is the named Go type (consumed by the `inline`/`array` keywords); pos drives diagnostics.
//
// Reports back via the (handled, fallthrough) tuple:
//
//   - handled=false, fallthrough=false → no swagger:type present
//   - handled=true,  fallthrough=false → resolved (caller returns nil)
//   - handled=true,  fallthrough=true  → unresolved value (file / unknown;
//     a diagnostic was recorded — caller falls through to the underlying type)
func (s *Builder) classifierNamedTypeOverride(cg *ast.CommentGroup, tgt ifaces.SwaggerTypable, ownType types.Type, pos token.Position) (handled, fallthroughUnderlying bool) {
	name, ok := s.findAnnotationArg(cg, grammar.AnnType)
	if !ok {
		return false, false
	}
	if s.resolveTypeOverride(name, tgt, ownType, pos) {
		if sf, hasStrfmt := s.findAnnotationArg(cg, grammar.AnnStrfmt); hasStrfmt {
			s.applyStrfmtFormat(tgt.Schema(), sf, pos)
		}
		return true, false
	}
	return true, true
}

// enumName resolves the enum type name for a swagger:enum annotation: the explicit `swagger:enum
// <Name>` argument, or — for the BARE `swagger:enum` form on a type declaration — the declared
// type's own name (F4b, the name is redundant on a type decl).
//
// Returns ("", false) when no swagger:enum is present, or when the bare form has no declared type
// to infer from.
func (s *Builder) enumName(cg *ast.CommentGroup, declTypeName string) (string, bool) {
	if name, ok := s.findAnnotationArg(cg, grammar.AnnEnum); ok {
		return name, true
	}
	if declTypeName != "" && s.findAnnotation(cg, grammar.AnnEnum) != nil {
		return declTypeName, true
	}
	return "", false
}

// classifierNamedBasic is the named-type walker for `buildNamedBasic`.
//
// Consumes a cascade of classifier annotations in source-priority order:
//
//	swagger:strfmt → swagger:enum → swagger:default → swagger:type
//
// The final arm is the "primitive-inline" branch, which now fires only in SimpleSchema mode (the M1
// contract for non-body parameters and response headers — `$ref` forbidden by OAS v2 so the
// underlying primitive ships inline).
//
// `swagger:alias` USED to be a second trigger here (a per-type force-inline override); it is
// deprecated (F8) and no longer affects output — its presence only emits a deprecation
// diagnostic.
//
// Returns:
//   - handled=true  → caller returns nil (target written, terminal)
//   - handled=false → no classifier matched; caller continues to
//     FindModel / SwaggerSchemaForType fallback
//
// recordEnumOrigins anchors each enum value to its const source position, but only when this is the
// canonical definition node (s.path set; cleared in field context, where the enum is a $ref to that
// definition instead).
// No-op when no provenance sink is wired.
//
// Cross-ref linkage only.
func (s *Builder) recordEnumOrigins(enumPos []token.Pos) {
	if s.path == "" || !s.Ctx.OriginEnabled() {
		return
	}
	for i, pos := range enumPos {
		s.Ctx.RecordOrigin(s.path+scanner.JSONPointer("enum", strconv.Itoa(i)), s.Ctx.PosOf(pos))
	}
}

// applyEnum writes the enum members of enumName onto tgt, together with the type/format of the
// enum's own declared Go type.
//
// Type and format come from utitpe — the DECLARED underlying basic type — never from the values.
// The scanner hands over evaluated constants, so every integer width collapses to int64 and every
// float width to float64 on the way out; typing the schema from the first value made an
// `int8` enum an `int64` one, a `float32` enum a `double`, and — when a float enum's first member
// happened to be written as an integer literal (`= 0`) — produced `{type: integer}` carrying
// fractional members, a schema no validator can satisfy. Each value is then normalised to that
// declared type (go-swagger#3412 follow-up).
//
// Returns false when the enum has no usable member, leaving tgt untouched so the caller can fall
// through to the type-resolution engine.
//
// # Details
//
// See [§enum-typing](./README.md#enum-typing) — why the declared type wins over the parsed values,
// and the two defects that rule settles.
func (s *Builder) applyEnum(pkg *packages.Package, declared *types.Named, utitpe *types.Basic, tgt ifaces.SwaggerTypable, enumName string) (resolved bool) {
	enumValues, enumDesces, enumPos, _ := s.Ctx.FindEnumValues(pkg, enumName)
	if len(enumValues) == 0 {
		return false
	}

	schemaType := basicSchemaType(utitpe)
	values := make([]any, 0, len(enumValues))
	descs := make([]string, 0, len(enumValues))
	positions := make([]token.Pos, 0, len(enumValues))

	for i, raw := range enumValues {
		value, ok := validations.CoerceConstant(raw, schemaType)
		if !ok {
			// Unreachable from code that compiles: Go rejects a const whose value does not fit its
			// declared type. Reported rather than emitted, so a malformed member can never contradict
			// the schema's own type.
			s.RecordDiagnostic(grammar.Warnf(s.declPos(), grammar.CodeInvalidEnumOption,
				"swagger:enum %s: const value %v is not representable as %s; member dropped",
				enumName, raw, utitpe.Name()))
			continue
		}

		values = append(values, value)
		if i < len(enumDesces) {
			descs = append(descs, enumDesces[i])
		}
		if i < len(enumPos) {
			positions = append(positions, enumPos[i])
		}
	}

	if len(values) == 0 {
		return false
	}

	if !s.applyEnumType(declared, utitpe, tgt, schemaType, enumName) {
		return false
	}

	tgt.WithEnum(values...)
	if len(descs) > 0 {
		tgt.WithEnumDescription(strings.Join(descs, "\n"))
	}
	s.recordEnumOrigins(positions)

	return true
}

// applyEnumType writes the enum's `type` / `format` onto tgt, resolved from the enum's declared Go
// type.
//
// The underlying basic type is the usual answer, but it is not always the whole one: a type
// declared OVER another named type (`type Kind strfmt.UUID`) reaches go/types with `string` as its
// underlying, and everything the intermediate contributed — here the `uuid` format — is gone from
// that view. The same type without the enum annotation keeps its format, because the ordinary path
// resolves the declaration's right-hand side rather than its underlying; inheritedStrfmt is the
// enum arm's equivalent of that, restricted to what a strfmt can legally decorate (a string).
//
// Reports false when the underlying type has no Swagger representation at all (a complex64 enum):
// there is no schema to write members onto, and the caller falls through so the type-resolution
// engine reports the unsupported type.
func (s *Builder) applyEnumType(declared *types.Named, utitpe *types.Basic, tgt ifaces.SwaggerTypable, schemaType, enumName string) bool {
	if schemaType == "string" {
		if format, ok := s.inheritedStrfmt(declared); ok {
			tgt.Typed("string", format)

			return true
		}
	}

	if err := resolvers.SwaggerSchemaForType(utitpe.Name(), tgt); err != nil {
		// e.g. a complex64 enum: no JSON representation, so there is no schema to write the members
		// onto. The type-resolution engine reports the unsupported type on the fallthrough.
		s.RecordDiagnostic(grammar.Warnf(s.declPos(), grammar.CodeUnsupportedGoType,
			"swagger:enum %s: underlying type %s has no Swagger representation: %v",
			enumName, utitpe.Name(), err))

		return false
	}

	return true
}

// inheritedStrfmt returns the strfmt format that declared inherits from the type it is written
// over, walking the chain of declarations to its right.
//
// `type Kind strfmt.UUID` carries no `swagger:strfmt` of its own — the annotation sits on
// `strfmt.UUID`, one declaration to the right — and go/types offers no way back to it, since
// Underlying() jumps straight to the bottom `string`. The AST does: a declaration's Spec.Type is
// the type it was written over, and the walk repeats from there, so an indirection chain several
// redefinitions long resolves like a single one.
//
// The walk stops at the first type that is not itself a named declaration (the basic bottom), at
// one the scan cannot see the source of, or at a repeat — Go forbids a cyclic type declaration, but
// nothing guarantees the AST handed to us came from code that compiles.
//
// Only `swagger:strfmt` is inherited: a type-changing annotation (`swagger:type`) on an intermediate
// would contradict the enum's own members rather than decorate them.
func (s *Builder) inheritedStrfmt(declared *types.Named) (string, bool) {
	seen := make(map[types.Type]struct{})

	for current := types.Type(declared); current != nil; {
		if _, dup := seen[current]; dup {
			return "", false
		}
		seen[current] = struct{}{}

		decl, ok := s.Ctx.DeclForType(current)
		if !ok || decl == nil || decl.Spec == nil || decl.Pkg == nil {
			return "", false
		}

		// The enum's own declaration is the first step: its swagger:strfmt (if any) already won in
		// classifierNamedBasic's strfmt-first arm, so a match here can only come from further right.
		if current != types.Type(declared) {
			if format, ok := s.findAnnotationArg(decl.Comments, grammar.AnnStrfmt); ok {
				return format, true
			}
		}

		rhs, ok := decl.Pkg.TypesInfo.Types[decl.Spec.Type]
		if !ok {
			return "", false
		}

		switch rhs.Type.(type) {
		case *types.Named, *types.Alias:
			current = rhs.Type
		default:
			return "", false
		}
	}

	return "", false
}

// basicSchemaType maps a Go basic type to the Swagger type whose value domain it belongs to.
//
// Driven by go/types' own kind bits rather than a name table, so every integer width (including
// `byte` / `rune`) lands on "integer" without enumerating them. Types with no Swagger value domain
// (complex, unsafe.Pointer) yield "" — the caller's cue to leave values untouched.
func basicSchemaType(utitpe *types.Basic) string {
	switch info := utitpe.Info(); {
	case info&types.IsInteger != 0:
		return "integer"
	case info&types.IsFloat != 0:
		return "number"
	case info&types.IsString != 0:
		return "string"
	case info&types.IsBoolean != 0:
		return "boolean"
	default:
		return ""
	}
}

func (s *Builder) classifierNamedBasic(cg *ast.CommentGroup, pkg *packages.Package, declared *types.Named, utitpe *types.Basic, tgt ifaces.SwaggerTypable) (resolved bool) {
	if name, ok := s.findAnnotationArg(cg, grammar.AnnStrfmt); ok {
		tgt.Typed("string", name)
		return true
	}

	if enumName, ok := s.enumName(cg, declared.Obj().Name()); ok {
		if s.applyEnum(pkg, declared, utitpe, tgt, enumName) {
			return true
		}
		// swagger:enum with no matching const values.
		// Fall through so the type-resolution engine can still decide what to do with the underlying Go
		// type (it may be a model, an alias, a strfmt, …) rather than dropping the field entirely.
		s.RecordDiagnostic(grammar.Warnf(s.declPos(), grammar.CodeInvalidEnumOption,
			"swagger:enum %s: no matching const values found; enum semantics dropped", enumName))
	}

	if _, ok := s.findAnnotationArg(cg, grammar.AnnDefaultName); ok {
		return true
	}

	if typeName, ok := s.findAnnotationArg(cg, grammar.AnnType); ok {
		_ = resolvers.SwaggerSchemaForType(typeName, tgt)
		return true
	}

	// swagger:alias is DEPRECATED (F8): it is now an empty sink.
	// It used to force a named primitive to inline ({type:string}) instead of the $ref a swagger:model
	// primitive gets; that special behaviour is removed — the type now follows default handling.
	//
	// Emit a deprecation diagnostic and fall through. (Detection lives here because this is the only
	// site that ever consulted swagger:alias; on non-primitive types it was already inert.)
	if alias := s.findAnnotation(cg, grammar.AnnAlias); alias != nil {
		s.RecordDiagnostic(grammar.Warnf(alias.Pos(), grammar.CodeDeprecated,
			`swagger:alias is deprecated and no longer affects output; use "swagger:type inline" to inline a type, or swagger:model for a first-class definition`))
	}

	// SimpleSchema mode (non-body params / response headers) still inlines the underlying primitive
	// — $ref is forbidden there by OAS v2.
	if s.simpleSchema {
		if err := resolvers.SwaggerSchemaForType(utitpe.Name(), tgt); err == nil {
			return true
		}
	}

	return false
}

// classifierNamedArrayLike is the named-type walker shared between `buildNamedArray` and
// `buildNamedSlice`.
//
// Both have the same classifier surface — `swagger:strfmt` and `swagger:type` — with subtly
// different strfmt fall-throughs (array honors a "bsonobjectid" special case the slice doesn't).
// The boolean `forSlice` switches that arm; the rest is identical.
//
// Returns:
//   - handled=true,  err=nil   → caller returns nil
//   - handled=true,  err!=nil  → unrecognised swagger:type → caller
//     should fall through to inline the element type
//   - handled=false, err=nil   → no classifier matched
func (s *Builder) classifierNamedArrayLike(cg *ast.CommentGroup, tgt ifaces.SwaggerTypable, forSlice bool) (handled bool, fallthroughElement bool) {
	if sfnm, isf := s.findAnnotationArg(cg, grammar.AnnStrfmt); isf {
		if sfnm == "byte" {
			tgt.Typed("string", sfnm)
			return true, false
		}
		if !forSlice && sfnm == "bsonobjectid" {
			tgt.Typed("string", sfnm)
			return true, false
		}
		tgt.Items().Typed("string", sfnm)
		return true, false
	}

	// When swagger:type is set to an unsupported value (e.g. "array"), skip the $ref and inline the
	// array/slice schema with the proper items type.
	if tn, ok := s.findAnnotationArg(cg, grammar.AnnType); ok {
		if err := resolvers.SwaggerSchemaForType(tn, tgt); err != nil {
			return true, true
		}
		return true, false
	}

	return false, false
}

// classifierAliasTargetStrfmt is the named-type walker fired from `buildNamedAllOf`'s struct branch
// — checks the alias's target type's docstring for `swagger:strfmt`.
//
// On match writes `{string, <format>}` to schema and returns true.
func (s *Builder) classifierAliasTargetStrfmt(tpe types.Type, tgt ifaces.SwaggerTypable) bool {
	decl, ok := s.Ctx.DeclForType(tpe)
	if !ok || decl == nil {
		return false
	}
	if name, ok := s.findAnnotationArg(decl.Comments, grammar.AnnStrfmt); ok {
		tgt.Typed("string", name)
		return true
	}
	return false
}

// fieldDoc is the field-level FieldWalker output: every classifier signal a struct field /
// interface method might carry, pre-extracted in a single pass over the field's ParseBlocks slice.
//
// Consumed by the four field-level call patterns documented on scanFieldDoc.
type fieldDoc struct {
	// Ignored — bare `swagger:ignore` presence (the field / method should be skipped entirely).
	Ignored bool
	// JSONName — argument of `swagger:name <X>` (rename the JSON property name).
	//
	// Empty when the annotation is absent or its arg is empty / multi-word.
	JSONName string
	// StrfmtName — argument of `swagger:strfmt <X>` (the string format to inline).
	//
	// Empty when the annotation is absent or its arg is empty / multi-word.
	StrfmtName string
	// TypeOverride — argument of `swagger:type <X>` at the field level.
	//
	// Empty when absent or multi-word.
	// See [§user-overrides](./README.md#user-overrides).
	TypeOverride string
	// IsAllOfMember — bare `swagger:allOf` presence (treat this embedded type as an allOf compound
	// member).
	IsAllOfMember bool
	// AllOfClass — argument of `swagger:allOf <X>` (the discriminator class for x-class).
	//
	// Empty for bare `swagger:allOf`.
	AllOfClass string
	// OmitTargets — arguments of `swagger:omit <name>[,<name>…]` written ON AN EMBED: the Go field
	// names not to promote out of the embedded type.
	//
	// Nil when the annotation is absent. See omit.go.
	OmitTargets []string
}

// scanFieldDoc inspects afld's docstring through the ParseBlocks cache and returns every
// field-level classifier signal in one pass.
//
// Consumes: swagger:ignore / swagger:name / swagger:strfmt / swagger:type / swagger:allOf.
// The AnnType arm carries an inline single-word filter — see
// [§user-overrides](./README.md#user-overrides) for why.
func (s *Builder) scanFieldDoc(afld *ast.Field) fieldDoc {
	var fd fieldDoc
	if afld == nil {
		return fd
	}
	var nameKeyword string
	for _, b := range s.ParseBlocks(afld.Doc) {
		switch b.AnnotationKind() { //nolint:exhaustive // field-level walker only consumes these six kinds
		case grammar.AnnIgnore:
			fd.Ignored = true
		case grammar.AnnName:
			if name, ok := b.AnnotationArg(); ok {
				fd.JSONName = name
			}
		case grammar.AnnStrfmt:
			if name, ok := b.AnnotationArg(); ok {
				fd.StrfmtName = name
			}
		case grammar.AnnType:
			if name, ok := b.AnnotationArg(); ok && !strings.ContainsAny(name, " \t") {
				fd.TypeOverride = name
			}
		case grammar.AnnAllOf:
			fd.IsAllOfMember = true
			if name, ok := b.AnnotationArg(); ok {
				fd.AllOfClass = name
			}
		case grammar.AnnOmit:
			if arg, ok := b.AnnotationArg(); ok {
				fd.OmitTargets = append(fd.OmitTargets, parseOmitTargets(arg)...)
			}
		}
		// The `name:` keyword is the canonical field-naming keyword, honoured uniformly across schema /
		// param / header (doc-quirk G2).
		// On a model property or interface method it renames the emitted JSON name just as it already
		// does on a parameter / header field.
		//
		// It takes precedence over the legacy swagger:name annotation when both are present, so capture
		// it across all blocks and apply it last.
		if v, ok := b.GetString(grammar.KwName); ok {
			if v = strings.TrimSpace(v); v != "" {
				nameKeyword = v
			}
		}
	}
	if nameKeyword != "" {
		fd.JSONName = nameKeyword
	}
	return fd
}

// classifierStructPreBuildType is the named-type walker fired at the top of `buildFromStruct`.
//
// Consumes only `swagger:type`.
// On match attempts SwaggerSchemaForType (errors are swallowed — the caller's fallback handles
// unknown leaves) and returns true to short-circuit the struct-build pipeline.
func (s *Builder) classifierStructPreBuildType(cg *ast.CommentGroup, tgt ifaces.SwaggerTypable) (resolved bool) {
	name, ok := s.findAnnotationArg(cg, grammar.AnnType)
	if !ok {
		return false
	}
	_ = resolvers.SwaggerSchemaForType(name, tgt)
	return true
}

// classifierNamedStructStrfmt is the named-type walker fired from `buildNamedStruct`'s strfmt-first
// branch.
//
// Checks the struct's docstring for `swagger:strfmt`; on match writes `{string, <format>}` to tgt
// and returns true.
//
// Pre-FindModel call site: matters because FindModel registers the type in ExtraModels as a side
// effect; running this walker first prevents an orphan top-level definition for a strfmt- inlined
// type.
func (s *Builder) classifierNamedStructStrfmt(cg *ast.CommentGroup, tgt ifaces.SwaggerTypable) (resolved bool) {
	if name, ok := s.findAnnotationArg(cg, grammar.AnnStrfmt); ok {
		tgt.Typed("string", name)
		return true
	}
	return false
}
