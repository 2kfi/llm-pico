# Layout Reference

> Auto-extracted from live DOM. Use this to understand how the site is structured spatially.

## Spacing System

**Base grid:** 5px

**Scale:** `5, 10, 15, 20, 25, 30, 35, 40, 45, 50, 55, 60, 65, 70, 75` px

| Spacing | Semantic Use |
|---------|-------------|
| 5px | Tight — within a component |
| 10px | Medium — between sibling items |
| 20px | Wide — between sections |
| 40px | Vast — major section breaks |

## Flex Layouts

| Element | Direction | Justify | Align | Gap | Children |
|---------|-----------|---------|-------|-----|----------|
| `nav.zOQ0BU6AULAqd7Qd` | column | — | — | 8px | 1 |
| `div.main-view-container__scroll-node.qLlzLghnvcCySJ8T` | row | — | stretch | — | 3 |
| `a.e-10451-legacy-button.e-10451-legacy-button-tertiary` | row | center | center | — | 2 |

## Structural Containers

### `<nav>` (`nav.zOQ0BU6AULAqd7Qd`)

```
display:          flex
flex-direction:   column
justify-content:  —
align-items:      —
gap:              8px
children:         1
```

### `<main>` (`main.J6wP3V0xzh0Hj_MS`)

```
display:          block
children:         2
```

## Layout Rules

- **Container max-width:** `100%` — always center with `margin: auto`
- Primary layout system: **Flexbox**
- Every spacing value must be a multiple of **5px**
- Never use arbitrary margin/padding values outside the spacing scale

