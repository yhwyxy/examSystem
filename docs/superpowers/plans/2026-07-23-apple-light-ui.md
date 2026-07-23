# Apple Light UI Refresh Implementation Plan

> **For agentic workers:** Execute task-by-task. Steps use checkbox syntax for tracking.

**Goal:** Apply Apple Light design tokens and component polish across the entire static frontend without changing business logic.

**Architecture:** Single shared stylesheet (`frontend/css/style.css`) owns design tokens and all page skins. HTML pages keep existing class hooks; only CSS is rewritten for visual language.

**Tech Stack:** Static HTML + CSS + vanilla JS (no new deps)

## Global Constraints

- Light polish only — CSS first; no JS/API changes
- No external font CDN; system font stack only
- Preserve all existing class names used by JS
- Create/use branch `ui/apple-light-refresh` before edits
- Respect `prefers-reduced-motion` for scale/transform transitions

---

### Task 1: Design tokens + global foundation

**Files:**
- Modify: `frontend/css/style.css` (`:root`, `body`, headings, links, muted)

- [ ] **Step 1:** Replace `:root` tokens with Apple Light palette (bg, ink, blue, radius, shadow)
- [ ] **Step 2:** Update body font stack, line-height, base colors
- [ ] **Step 3:** Update h1/h2/h3 scale and letter-spacing
- [ ] **Step 4:** Visual check: load `/` and `/admin` — canvas and type should match tokens

### Task 2: Core components

**Files:**
- Modify: `frontend/css/style.css` (`.btn`, inputs, `.panel`/`.card`, tables, badges, toast, focus rings)

- [ ] **Step 1:** Pill primary buttons, secondary outline, danger
- [ ] **Step 2:** Input focus rings using blue soft
- [ ] **Step 3:** Larger panel radius + lighter shadows
- [ ] **Step 4:** Table header/hover + badge pills aligned to semantic colors
- [ ] **Step 5:** Toast near-black Apple style

### Task 3: Shell pages (admin / login / detail)

**Files:**
- Modify: `frontend/css/style.css` (`.login-screen`, `.sidebar`, `.nav-item`, `.stat`, `.workspace`)

- [ ] **Step 1:** Login full-bleed soft gray + elevated card
- [ ] **Step 2:** Sidebar near-black with soft active states
- [ ] **Step 3:** Stats cards and workspace spacing
- [ ] **Step 4:** Confirm detail page inherits shell styles

### Task 4: Exam surface + specialized blocks

**Files:**
- Modify: `frontend/css/style.css` (`.exam-*`, questions, options, preview, exam-cards)

- [ ] **Step 1:** Exam shell max-width ~960px, hero/timer cards
- [ ] **Step 2:** Question cards + option selected/hover via `--blue-soft`
- [ ] **Step 3:** Align exam-run cards / editor nested panels to tokens
- [ ] **Step 4:** Add `@media (prefers-reduced-motion: reduce)` for transforms/animations

### Task 5: Verify + commit

- [ ] **Step 1:** Smoke open exam.html structure (static), admin shell CSS classes present
- [ ] **Step 2:** Commit docs + CSS on `ui/apple-light-refresh`

## Spec coverage

| Spec area | Task |
|-----------|------|
| Tokens | Task 1 |
| Buttons/inputs/panels/tables | Task 2 |
| Login/sidebar/admin | Task 3 |
| Exam + motion | Task 4 |
| Branch + commit | Task 5 |
