---
name: Sandman
description: Local portal for monitoring AFK agent launch records
---

<!-- SEED: re-run /impeccable document once there's code to capture the actual tokens and components. -->

# Design System: Sandman

## 1. Overview

**Creative North Star: "The Command Post"**

This system should feel like an ops console built for people who already know what they are looking at. It is pragmatic, sharp, and calm, with the visual discipline of terminal-native tooling and the legibility of a serious monitoring surface.

It rejects the generic SaaS dashboard look, especially floating KPI cards, decorative chrome, and anything that feels like polished theater. The interface must stay usable on mobile, but it should still read as a working control surface, not a marketing shell.

**Key Characteristics:**
- Dense, readable, and scannable
- Local-first and repo-scoped in feel
- Calm under load, never flashy
- Mobile-friendly without becoming watered down

## 2. Colors

The palette should be restrained: tinted neutrals, one deliberate accent, and enough semantic contrast to keep run state legible.

### Primary
- **Primary Accent**: [to be resolved during implementation]. Use sparingly for active runs, selected states, and key actions only.

### Neutral
- **Console Surface**: [to be resolved during implementation]. The main background and table canvas.
- **Raised Surface**: [to be resolved during implementation]. Secondary panels, side rails, and detail regions.
- **Text and Dividers**: [to be resolved during implementation]. Maintain strong contrast without harsh black-and-white extremes.

### Named Rules
**The One Accent Rule.** The accent color is reserved for state and action, not decoration. If it starts to feel like a dashboard theme, it is already wrong.

## 3. Typography

**Body Font:** Single sans, with a technical, neutral feel.
**Label/Mono Font:** Monospace for run metadata, paths, commands, and log-like content.

**Character:** The type system should feel precise, compact, and calm. Sans carries the interface, mono carries the machine details.

### Hierarchy
- **Headline**: Strong but not oversized, used for page sections and active context.
- **Title**: Compact section labels and row titles.
- **Body**: Dense but readable, with a 65 to 75ch target for prose.
- **Label**: Small, crisp, and utilitarian for status and metadata.

### Named Rules
**The Terminal Split Rule.** Use sans for navigation and comprehension, mono for data that benefits from exactness.

## 4. Elevation

The system should stay mostly flat, with depth coming from tone changes, borders, and layering rather than heavy shadow. Any elevation should feel functional, like a panel coming forward because it is active or expanded.

### Named Rules
**The Flat-By-Default Rule.** Surfaces stay calm until state demands emphasis.

## 5. Do's and Don'ts

### Do:
- **Do** keep the main table and detail views readable on small screens.
- **Do** use one clear accent for active state, selection, and primary actions.
- **Do** preserve the ops-console tone, with dense information arranged predictably.

### Don't:
- **Don't** use generic SaaS dashboard language or visuals.
- **Don't** lean on floating KPI cards, decorative chrome, or repeated card-grid layouts.
- **Don't** make the portal feel flashy, speculative, or brand-first.
- **Don't** turn routine inspection into a modal-heavy flow.
