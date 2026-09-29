# ADR-0016: the dashboard's visual identity

- **Status:** Proposed
- **Date:** 2026-09-29
- **Authors:** onedr0p.
- **Amends:** [ADR-0009](0009-web-dashboard.md) §2.1 (Geist and Geist
  Mono, copied from konflate).
- **Amended by:** [ADR-0017](0017-dashboard-sections.md), which replaces
  the layout this ADR left as ADR-0009 set it.

> Scope: the dashboard's typefaces, palette and the few treatments built
> on them. The layout, the components and konflate's frontend stack
> otherwise stay as ADR-0009 set them.

## 1. Context

ADR-0009 copied konflate's stack whole, its typefaces included, so the
dashboard looked like konflate's, and like most developer dashboards:
Geist, a grey ground, a blue accent, uppercase labels and fully rounded
pills. Nothing in it said what kritik does, which is read a change and mark
the lines it has something to say about.

## 2. Decision

The dashboard looks like a marked-up proof.

- **Type:** Schibsted Grotesk, a news grotesk made for a Scandinavian
  publisher, for the interface and prose, and Red Hat Mono for code and
  identifiers. Red Hat Mono draws `==` and `<=` as typed; the monospaced
  faces that ligate them by default were left out, since a reviewer must
  see the characters the diff holds. Both are self-hosted through
  `@fontsource-variable`, as the CSP requires.
- **Palette:** ink on proof paper. The chrome sits on the paper and the
  content on lighter sheets; links are a fountain-pen blue, blocking
  findings red pencil, important ones ochre. The dark theme is the same
  roles in ink, not a separate design. Every text colour meets 4.5:1 on
  each surface it sits on, in both themes.
- **The highlighter** is the one signature: the wordmark, the current
  page in the navigation, the row the keyboard has selected, and the line
  number of a diff line a finding is anchored to. It only ever sits behind
  text. It stays under 3:1 on the light sheet, so each state it marks also
  carries an ink edge that contrasts on its own. In the dark theme it is
  dimmed, as an e-reader's night highlight is, and the text on it stays
  light.
- **Labels** are in sentence case, not uppercase and tracked. Radii follow
  what a shape is: marks such as pills and counts 3 to 4px, controls 6px,
  surfaces 8px, overlays 10px.

## 3. Consequences

- The tokens in `app.css` stay the only place a colour is named; the
  highlighter adds `--mark`, `--mark-wash` and `--edge`, and a primary
  button's text is `--on-accent`, since the dark theme's accent is too
  light for white text.
- A new state that marks something kritik has an opinion on uses the
  highlighter; other states keep the accent.

## 4. Rejected alternatives

- **Keep konflate's look.** It costs nothing, but ADR-0009's reason to
  copy the stack was not to redesign its patterns, and the typefaces and
  palette carry none of them.
- **A bright highlighter in the dark theme with dark text on it.** The
  text outside the band, such as a letter's ascender, would then be dark on
  the dark ground.
