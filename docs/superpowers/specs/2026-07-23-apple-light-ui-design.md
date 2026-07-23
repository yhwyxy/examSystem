# Apple Light UI Refresh — Design Spec

**Date:** 2026-07-23  
**Status:** Approved  
**Scope:** Full frontend (exam, admin login, admin shell, detail review)

## Goal

Unify the exam system UI under an **Apple Light** visual language: soft gray canvas (`#f5f5f7`), near-black type (`#1d1d1f`), blue primary actions (`#0071e3`), generous but practical spacing, light shadows, system typography — closer to apple.com light marketing pages than a heavy enterprise admin theme.

## Constraints

- **Approach:** Light polish — primarily CSS tokens and component styles
- **Primary file:** `frontend/css/style.css`
- **Optional:** Minimal HTML class tweaks only if needed for hierarchy
- **Out of scope:** Business logic, APIs, JS flows, new frameworks/UI libraries, external font CDNs
- **Branch:** Create git branch before code changes

## Design tokens

| Token | Value | Role |
|-------|--------|------|
| `--bg` | `#f5f5f7` | Page canvas |
| `--bg-soft` | `#fafafa` | Secondary surfaces / table header |
| `--surface` | `#ffffff` | Cards / panels |
| `--surface-muted` | `#f5f5f7` | Nested muted fills |
| `--ink` | `#1d1d1f` | Primary text |
| `--ink-soft` | `#424245` | Labels / secondary titles |
| `--muted` | `#86868b` | Helper / captions |
| `--line` | `rgba(0,0,0,.08)` | Dividers |
| `--line-strong` | `rgba(0,0,0,.12)` | Input / table borders |
| `--blue` | `#0071e3` | Primary action / links |
| `--blue-strong` | `#0077ed` | Hover |
| `--blue-soft` | `rgba(0,113,227,.08)` | Selection / soft emphasis |
| `--teal` | `#6e6e73` | Eyebrow (muted Apple gray) |
| `--green` / `--amber` / `--red` | Calm semantic | Status badges |
| `--sidebar` | `#1d1d1f` | Admin nav |
| `--sidebar-soft` | `#2d2d2f` | Nav hover/active |
| `--sidebar-muted` | `#a1a1a6` | Nav captions |
| `--radius` | `12px` | Controls |
| `--radius-lg` | `18px` | Cards / panels |
| `--shadow` | `0 2px 8px rgba(0,0,0,.04)` | Default elevation |
| `--shadow-hover` | `0 4px 16px rgba(0,0,0,.06)` | Hover elevation |

## Typography

```text
font-family: -apple-system, BlinkMacSystemFont, "SF Pro Text", "PingFang SC",
  "Helvetica Neue", "Microsoft YaHei", sans-serif;
```

- **h1:** `clamp(28px, 3.2vw, 40px)`, weight 600–700, `letter-spacing: -0.02em`
- **h2:** 20–22px, weight 600
- **Body:** 15–17px, weight 400, line-height ~1.47
- **Eyebrow:** 12px, weight 600, letter-spacing `0.06em`, muted
- **Timers / stats:** `font-variant-numeric: tabular-nums`

## Components

### Buttons
- Primary: pill (`border-radius: 980px`), blue fill, white text, min-height ~40–44px
- Secondary: white + hairline border + ink text
- Danger: red fill or red-tinted secondary treatment
- Focus: blue ring; active: subtle scale (respect `prefers-reduced-motion`)

### Inputs
- White fill, soft border, 12px radius
- Focus: blue border + `0 0 0 3px rgba(0,113,227,.25)` ring
- Labels: ink-soft, weight 500–600

### Cards / panels
- White, large radius, light border or soft shadow only
- Section heads: more breathing room, restrained titles

### Tables
- Soft header background, light row hover
- Slightly increased cell padding; status as pills

### Sidebar
- Near-black background, white/light nav text
- Soft active state; restrained brand type

### Login
- Full-screen `#f5f5f7`, centered white card, single-column form, full-width primary CTA

### Exam
- Centered shell ~920–980px
- Hero + timer as elevated white cards
- Options: hover/selected with `--blue-soft`

## Pages

1. **Exam (`exam.html`)** — canvas, hero, intake, questions, result
2. **Admin login** — centered card
3. **Admin shell** — sidebar + workspace stats/tables/cards
4. **Detail review** — same shell language as admin

## Non-goals

- Information architecture redesign
- New animation systems beyond light hover/active
- Dark mode

## Acceptance

- [ ] Shared tokens across exam / admin / detail
- [ ] Clear primary vs secondary vs danger hierarchy
- [ ] Tables remain scannable
- [ ] ~375px layouts do not break
- [ ] Visible focus rings; reduced-motion respected
- [ ] Smoke paths: login shell, paper list, exam start page open
