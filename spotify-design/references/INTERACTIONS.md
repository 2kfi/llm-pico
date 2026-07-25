# Interaction Reference

> Micro-interactions extracted from live DOM. Recreate these exactly for authentic feel.

## Coverage

| Component Type | Count | States Captured |
|----------------|-------|----------------|
| Button | 3 | default, hover, focus |
| Role Button | 3 | default, hover, focus |
| Link | 3 | default, hover, focus |
| Input | 3 | default, hover, focus |

## Transition System

These transition declarations were extracted from interactive elements:

```css
transition: background-color 0.2s ease-in-out, color 0.2s ease-in-out;
transition: color 0.22s ease-in;
transition: color 0.15s cubic-bezier(0.3, 0, 0, 1), transform 0.15s cubic-bezier(0.3, 0, 0, 1);
transition: all;
transition: background-color 0.15s cubic-bezier(0.3, 0, 0, 1), transform 0.15s cubic-bezier(0.3, 0, 0, 1);
transition: box-shadow 0.22s ease-in, background 0.22s ease-in, color 0.22s ease-in;
```

Apply these to all interactive elements. Never invent new durations or easings.

## Button Interactions

### Button 1 — `Home`

**States:**

- Default: `../screens/states/button-1-default.png`
- Hover: `../screens/states/button-1-hover.png`
- Focus: `../screens/states/button-1-focus.png`

**On hover:**

```css
/* background-color: rgb(31, 31, 31) → */ background-color: rgb(42, 42, 42);
/* transform: none → */ transform: matrix(1.04, 0, 0, 1.04, 0, 0);
```

**Transition:** `background-color 0.2s ease-in-out, color 0.2s ease-in-out`

### Button 2 — `Search`

**States:**

- Default: `../screens/states/button-2-default.png`
- Hover: `../screens/states/button-2-hover.png`
- Focus: `../screens/states/button-2-focus.png`

**On hover:**

```css
/* color: rgb(179, 179, 179) → */ color: rgb(255, 255, 255);
/* border-color: rgb(179, 179, 179) → */ border-color: rgb(255, 255, 255);
/* transform: none → */ transform: matrix(1.04, 0, 0, 1.04, 0, 0);
/* outline: rgb(179, 179, 179) none 3px → */ outline: rgb(255, 255, 255) none 3px;
/* outline-color: rgb(179, 179, 179) → */ outline-color: rgb(255, 255, 255);
```

**On focus:**

```css
/* color: rgb(179, 179, 179) → */ color: rgb(255, 255, 255);
/* border-color: rgb(179, 179, 179) → */ border-color: rgb(255, 255, 255);
/* outline: rgb(179, 179, 179) none 3px → */ outline: rgb(255, 255, 255) none 3px;
/* outline-color: rgb(179, 179, 179) → */ outline-color: rgb(255, 255, 255);
```

**Transition:** `color 0.22s ease-in`

### Button 3 — `Browse`

**States:**

- Default: `../screens/states/button-3-default.png`
- Hover: `../screens/states/button-3-hover.png`
- Focus: `../screens/states/button-3-focus.png`

**On hover:**

```css
/* color: rgb(179, 179, 179) → */ color: rgb(255, 255, 255);
/* border-color: rgb(179, 179, 179) → */ border-color: rgb(255, 255, 255);
/* transform: none → */ transform: matrix(1.04, 0, 0, 1.04, 0, 0);
/* outline: rgb(179, 179, 179) none 3px → */ outline: rgb(255, 255, 255) none 3px;
/* outline-color: rgb(179, 179, 179) → */ outline-color: rgb(255, 255, 255);
/* transition: color 0.15s cubic-bezier(0.3, 0, 0, 1), transform 0.15s cubic-bezier(0.3, 0, 0, 1) → */ transition: color 0.05s cubic-bezier(0.3, 0, 0, 1), transform 0.05s cubic-bezier(0.3, 0, 0, 1);
```

**Transition:** `color 0.15s cubic-bezier(0.3, 0, 0, 1), transform 0.15s cubic-bezier(0.3, 0, 0, 1)`

## Role Button Interactions

### Role Button 1 — `div`

**States:**

- Default: `../screens/states/role-button-1-default.png`
- Hover: `../screens/states/role-button-1-hover.png`
- Focus: `../screens/states/role-button-1-focus.png`

**Transition:** `all`

_No visible style changes detected for this element._

### Role Button 2 — `div`

**States:**

- Default: `../screens/states/role-button-2-default.png`
- Hover: `../screens/states/role-button-2-hover.png`
- Focus: `../screens/states/role-button-2-focus.png`

**Transition:** `all`

_No visible style changes detected for this element._

### Role Button 3 — `div`

**States:**

- Default: `../screens/states/role-button-3-default.png`
- Hover: `../screens/states/role-button-3-hover.png`
- Focus: `../screens/states/role-button-3-focus.png`

**Transition:** `all`

_No visible style changes detected for this element._

## Link Interactions

### Link 1 — `a`

**States:**

- Default: `../screens/states/link-1-default.png`
- Hover: `../screens/states/link-1-hover.png`
- Focus: `../screens/states/link-1-focus.png`

**On hover:**

```css
/* text-decoration: none → */ text-decoration: underline;
```

**On focus:**

```css
/* text-decoration: none → */ text-decoration: underline;
```

**Transition:** `all`

### Link 2 — `Install App`

**States:**

- Default: `../screens/states/link-2-default.png`
- Hover: `../screens/states/link-2-hover.png`
- Focus: `../screens/states/link-2-focus.png`

**On hover:**

```css
/* color: rgb(179, 179, 179) → */ color: rgb(255, 255, 255);
/* border-color: rgb(179, 179, 179) → */ border-color: rgb(255, 255, 255);
/* transform: none → */ transform: matrix(1.04, 0, 0, 1.04, 0, 0);
/* outline: rgb(179, 179, 179) none 3px → */ outline: rgb(255, 255, 255) none 3px;
/* outline-color: rgb(179, 179, 179) → */ outline-color: rgb(255, 255, 255);
/* transition: color 0.15s cubic-bezier(0.3, 0, 0, 1), transform 0.15s cubic-bezier(0.3, 0, 0, 1) → */ transition: color 0.05s cubic-bezier(0.3, 0, 0, 1), transform 0.05s cubic-bezier(0.3, 0, 0, 1);
```

**Transition:** `color 0.15s cubic-bezier(0.3, 0, 0, 1), transform 0.15s cubic-bezier(0.3, 0, 0, 1)`

### Link 3 — `Browse podcasts`

**States:**

- Default: `../screens/states/link-3-default.png`
- Hover: `../screens/states/link-3-hover.png`
- Focus: `../screens/states/link-3-focus.png`

**On hover:**

```css
/* transform: none → */ transform: matrix(1.04, 0, 0, 1.04, 0, 0);
/* transition: background-color 0.15s cubic-bezier(0.3, 0, 0, 1), transform 0.15s cubic-bezier(0.3, 0, 0, 1) → */ transition: background-color 0.05s cubic-bezier(0.3, 0, 0, 1), transform 0.05s cubic-bezier(0.3, 0, 0, 1);
```

**Transition:** `background-color 0.15s cubic-bezier(0.3, 0, 0, 1), transform 0.15s cubic-bezier(0.3, 0, 0, 1)`

## Input Interactions

### Input 1 — `What do you want to play?`

**States:**

- Default: `../screens/states/input-1-default.png`
- Hover: `../screens/states/input-1-hover.png`
- Focus: `../screens/states/input-1-focus.png`

**On hover:**

```css
/* background-color: rgb(31, 31, 31) → */ background-color: rgb(42, 42, 42);
/* box-shadow: none → */ box-shadow: rgba(255, 255, 255, 0.1) 0px 0px 0px 1px inset;
```

**On focus:**

```css
/* background-color: rgb(31, 31, 31) → */ background-color: rgb(42, 42, 42);
/* box-shadow: none → */ box-shadow: rgb(255, 255, 255) 0px 0px 0px 2px inset;
```

**Transition:** `box-shadow 0.22s ease-in, background 0.22s ease-in, color 0.22s ease-in`

### Input 2 — `range`

**States:**

- Default: `../screens/states/input-2-default.png`
- Hover: `../screens/states/input-2-hover.png`
- Focus: `../screens/states/input-2-focus.png`

**Transition:** `all`

_No visible style changes detected for this element._

### Input 3 — `range`

**States:**

- Default: `../screens/states/input-3-default.png`
- Hover: `../screens/states/input-3-hover.png`
- Focus: `../screens/states/input-3-focus.png`

**Transition:** `all`

_No visible style changes detected for this element._

## Interaction Rules

- Hover effects include **color transitions** — use the extracted values, not approximations
- Focus states use **outline** (not box-shadow) — always match the extracted focus ring
- Transition durations in use: `0.2s`, `0.22s`, `0.15s`
- Always respect `prefers-reduced-motion` — set all transitions to `0s` when enabled

