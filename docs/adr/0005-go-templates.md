# ADR-0005: comment templates are Go templates with sprout

- **Status:** Proposed
- **Date:** 2026-09-25
- **Amends:** [ADR-0003](0003-forgejo-agentic-review.md) §2.4, the
  rendering of the sticky and inline comments. The contract, the defaults'
  content, the fallback rule and the marker are unchanged.
- **Authors:** onedr0p.

## 1. Context

ADR-0003 renders comments from Jinja2 templates through gonja, and lets a
repository replace either template from `.kritik.yaml`. A repository
template runs in the worker, which serves every tenant, so it must not be
able to stall or exhaust that process. gonja was not built for untrusted
templates: it renders several constructs into private, uncapped buffers,
and its expression language has operators (`+`, `~`, `*`, `**`) that build
a large value in one step. Bounding it took a sandbox of about 800 lines
that rewrote the parse tree, replaced `set`, filtered control structures,
filters and methods through allowlists, and estimated the allocation of
each filter. That code is the largest single file in the service and the
one most exposed to a gonja release changing something it relies on.

Every other Go service in the fleet that renders operator text, tuppr and
chaski among them, uses the standard library's `text/template` with the
[sprout](https://github.com/go-sprout/sprout) function registries, and a
Go template author in this organisation has that syntax in every Helm
chart they maintain.

## 2. Decision

Comments render with `text/template`. The summary template's dot is the
review (`RenderData`), the inline template's dot is one finding, so
templates read fields by their Go names: `.Result.Summary.Take`,
`.Counts.Blocking`, `.Path`. The functions are sprout's std, strings,
conversion, encoding, numeric, slices, maps, regex, time, semver and
reflect registries, the same set tuppr and chaski expose, less what
reaches outside the render (env, filesystem, network), what makes a render
unrepeatable (random, uniqueid), what has no use in a comment (checksum,
crypto) and `set`/`unset`, which mutate a dict in place and could make one
contain itself.

`text/template` has no operators: every value a template builds passes
through a function call, and a loop is the only way to repeat work. The
sandbox therefore needs three guards, enforced on a parse-tree walk and
through the function map:

- **Function calls.** Every function is wrapped. A call is refused when
  its arguments, plus an estimate of what the function allocates for the
  few that amplify their input (`repeat`, `indent`, `nindent`, `join`,
  `replace`, `regexReplaceAll`, `regexReplaceAllLiteral`, `seq`, `until`,
  `untilStep`, `printf`), exceed 256 KiB, and when its result does.
  Values are measured by walking them, counting every string and a fixed
  cost per element, so a structure that shares one large value many times
  is refused as if it were copied. `printf`, `print` and `println` are
  redefined so the wrapper sees them.
- **Loops.** The parse tree is rewritten so every `range` pipeline ends in
  a guard that charges the loop's items to a render-wide budget of 20,000
  iterations, charged in full when the loop starts so nested loops cannot
  multiply it.
- **Other templates.** `template`, `define` and `block` are refused: a
  template that invokes others can fan out exponentially with no loop and
  no output for the guards to see. Identifiers starting with `__kritik_`
  are reserved for the guards.

The output cap of 64 KiB, the two-second deadline, the fallback to the
default with a note, and the marker prepended outside the template are as
ADR-0003 has them. The deadline is checked on every write, call and loop,
so a render stops soon after it passes.

## 3. Consequences

**Positive.** The sandbox is a quarter of its former size, and its guards
follow from two properties of the language rather than from a list of
what gonja happens to do. Templates use the syntax the rest of the fleet
and every Helm chart use. One dependency of the fleet replaces one that
only kritik carried.

**Negative.** A repository template written for ADR-0003 as merged no
longer parses and falls back to the default with a note until it is
rewritten; no such template exists outside kritik's own tests. Go
templates are more verbose than Jinja2 for the same output.

## 4. Alternatives considered

- **Keep gonja and its sandbox.** Works today. Rejected for the size and
  fragility of the sandbox, and for being the one template language in the
  fleet that is not Go's.
- **`text/template` without sprout.** Fewer functions to bound, but no
  `trunc`, `join` or `indent`, which the defaults and any useful repository
  template need.
- **`html/template`.** Its contextual escaping is for HTML, not Markdown,
  and would mangle code in findings.
