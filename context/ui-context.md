# UI Context

## Experience Direction

Weave should feel like a calm, intelligent control room for serious work: fast, precise, collaborative, and trustworthy. It combines the focus of a modern code editor, the social presence of a multiplayer document, and the operational clarity of a production console.

The interface must avoid looking like another generic chatbot. Conversation is one input mode within a larger session containing people, agents, tasks, actions, files, approvals, evidence, and outcomes.

The developer-first release is desktop-first and defaults to dark mode. Light mode is fully supported for accessibility and personal preference. The design system must also be neutral enough to support future sales, legal, marketing, analysis, and operations workrooms.

## Design Principles

1. **Outcome before conversation:** show the task, state, progress, changes, and next decision before emphasizing chat.
2. **Trust through visibility:** users can always see what is happening, who initiated it, what it affects, and whether approval is required.
3. **Calm density:** display substantial technical information without visual noise.
4. **Progressive disclosure:** summarize first; reveal raw logs, command output, and provider detail on demand.
5. **Multiplayer presence:** collaborators and agent activity feel live without distracting animation.
6. **Safe control:** high-risk actions are visually distinct and explain impact before confirmation.
7. **Provider neutrality:** Claude Code, Codex, and future tools use a common interaction language; provider branding is secondary metadata.
8. **Accessible by default:** keyboard, screen reader, contrast, zoom, and reduced motion are first-class requirements.

## Brand Personality

- Confident, not loud
- Technical, not cryptic
- Premium, not ornamental
- Collaborative, not playful
- Intelligent, not anthropomorphic
- Trustworthy, not bureaucratic

## Colors

All product colors use semantic CSS custom properties. Components must not contain hardcoded color values.

### Dark Theme

| Role | CSS Variable | Value |
| --- | --- | --- |
| Page background | `--bg-base` | `#090B10` |
| Raised background | `--bg-raised` | `#0E1118` |
| Surface | `--bg-surface` | `#131722` |
| Surface hover | `--bg-surface-hover` | `#191E2B` |
| Surface selected | `--bg-surface-selected` | `#20263A` |
| Overlay | `--bg-overlay` | `#171B27` |
| Primary text | `--text-primary` | `#F4F6FB` |
| Secondary text | `--text-secondary` | `#B5BDCC` |
| Muted text | `--text-muted` | `#7D8798` |
| Disabled text | `--text-disabled` | `#555E6D` |
| Primary accent | `--accent-primary` | `#8B7CFF` |
| Primary hover | `--accent-primary-hover` | `#9E92FF` |
| Secondary accent | `--accent-secondary` | `#35D4B3` |
| Border default | `--border-default` | `#252B38` |
| Border strong | `--border-strong` | `#363E50` |
| Focus ring | `--focus-ring` | `#A99FFF` |
| Information | `--state-info` | `#62A8FF` |
| Success | `--state-success` | `#3DD6A1` |
| Warning | `--state-warning` | `#F3B95F` |
| Error | `--state-error` | `#FF6B7A` |
| Agent | `--identity-agent` | `#9A8CFF` |
| Human | `--identity-human` | `#51C7E8` |

### Light Theme

| Role | CSS Variable | Value |
| --- | --- | --- |
| Page background | `--bg-base` | `#F6F7FA` |
| Raised background | `--bg-raised` | `#FFFFFF` |
| Surface | `--bg-surface` | `#FFFFFF` |
| Surface hover | `--bg-surface-hover` | `#F0F2F7` |
| Surface selected | `--bg-surface-selected` | `#EAE8FF` |
| Overlay | `--bg-overlay` | `#FFFFFF` |
| Primary text | `--text-primary` | `#151821` |
| Secondary text | `--text-secondary` | `#4F5868` |
| Muted text | `--text-muted` | `#737D8F` |
| Disabled text | `--text-disabled` | `#A3A9B5` |
| Primary accent | `--accent-primary` | `#6658E8` |
| Primary hover | `--accent-primary-hover` | `#594CCF` |
| Secondary accent | `--accent-secondary` | `#087F6D` |
| Border default | `--border-default` | `#E1E4EA` |
| Border strong | `--border-strong` | `#C9CED8` |
| Focus ring | `--focus-ring` | `#6E60EE` |
| Information | `--state-info` | `#2878D0` |
| Success | `--state-success` | `#087F5B` |
| Warning | `--state-warning` | `#9A6100` |
| Error | `--state-error` | `#C7354D` |
| Agent | `--identity-agent` | `#6658E8` |
| Human | `--identity-human` | `#087F9C` |

Text and interactive-state pairings must meet WCAG 2.2 AA contrast. State components combine color with iconography and text.

## Typography

| Role | Font | Variable | Usage |
| --- | --- | --- | --- |
| UI text | Geist Sans | `--font-sans` | Navigation, forms, messages, metadata, headings |
| Code/mono | Geist Mono | `--font-mono` | Code, commands, logs, IDs, branch names, durations |

Type scale:

| Token | Size / Line height | Weight | Usage |
| --- | --- | --- | --- |
| `display-sm` | 32 / 40 | 650 | Onboarding and empty-state hero |
| `heading-lg` | 24 / 32 | 650 | Page title |
| `heading-md` | 18 / 26 | 600 | Panel and dialog title |
| `heading-sm` | 15 / 22 | 600 | Card and section title |
| `body-md` | 14 / 21 | 400 | Default product text |
| `body-sm` | 13 / 19 | 400 | Dense activity and metadata |
| `label` | 12 / 16 | 550 | Controls and compact labels |
| `code` | 13 / 20 | 400 | Code and terminal output |

Avoid all-uppercase labels except very short technical badges. Use sentence case throughout the product.

## Spacing and Density

Use a 4-pixel base grid. Preferred spacing tokens are 4, 8, 12, 16, 20, 24, 32, and 40 pixels.

- Compact rows: 32–36 px
- Default controls: 40 px
- Prominent controls: 44 px
- Page gutters: 20 px at compact desktop, 24–32 px at wide desktop
- Panel padding: 16 px for dense panels, 20–24 px for settings and onboarding
- Maximum reading width for prose: 720 px

Dense technical views may use compact spacing, but controls must remain keyboard and pointer accessible.

## Border Radius

| Context | Token | Value |
| --- | --- | --- |
| Badge / compact control | `--radius-sm` | `6px` |
| Input / button | `--radius-md` | `8px` |
| Card / panel | `--radius-lg` | `12px` |
| Dialog / popover | `--radius-xl` | `16px` |
| Pill / avatar | `--radius-full` | `999px` |

Avoid excessive rounding. Technical work surfaces should feel structured, not bubbly.

## Elevation

- Base panels use borders rather than shadows.
- Floating menus use a subtle shadow plus a strong border.
- Dialogs use a deeper shadow and restrained backdrop blur.
- Selected or active work surfaces use accent-tinted background and border, not glow.
- Terminal, code, and diff surfaces remain visually flat for readability.

## Component Library

Use shadcn/ui with Radix primitives and Tailwind. Components live in `apps/web/components/ui/`. Generated primitives should be wrapped or composed rather than repeatedly forked.

Required shared components include:

- Button, icon button, split button, and destructive button
- Input, textarea, combobox, select, checkbox, radio group, and switch
- Dialog, alert dialog, drawer, popover, dropdown menu, command menu, and tooltip
- Tabs, segmented control, accordion, collapsible, and resizable panels
- Avatar, avatar group, badge, status chip, and provider badge
- Toast and persistent inline alert
- Skeleton, spinner, progress bar, and step indicator
- Data table with sorting, filters, pagination, and empty state
- Code block, file tree, diff viewer, terminal output, and copy control
- Timeline event, action proposal, approval card, verification result, and artifact card

## Application Shell

The default authenticated layout contains:

- **Workspace rail:** 56 px collapsed rail for workspace switching and global creation
- **Primary sidebar:** 232 px, collapsible to 64 px; navigation and workspace context
- **Top bar:** 52 px; breadcrumbs, command palette, presence, notifications, help, and account menu
- **Main content:** flexible, scroll-owned by the active route

Primary navigation:

- Home
- Tasks
- Sessions
- Repositories
- Agents
- Team
- Usage
- Settings

The command palette opens with `Cmd/Ctrl + K` and supports navigation, task creation, session lookup, and safe session commands.

## Shared Session Room

The session room is the flagship experience.

### Desktop Layout

- **Header:** task title, status, repository/branch, provider, elapsed time, cost, participant avatars, and primary controls
- **Left panel (240–300 px):** plan, changed files, verification checks, artifacts, and session outline
- **Center panel (flexible, minimum 520 px):** chronological activity timeline with human, agent, tool, approval, and system events
- **Right panel (320–420 px):** context-sensitive inspector for action details, code diff, approvals, metadata, and comments
- **Composer:** anchored below the center panel for instructions, mentions, file references, and control shortcuts

Panels are resizable and remember user preference. At narrower widths, the right inspector becomes a drawer and the left panel collapses behind a tab.

### Activity Timeline

- Group noisy low-level events into expandable activity blocks.
- Show concise progress summaries by default.
- Clearly distinguish human instructions, agent updates, tool actions, approvals, and system events.
- Display actor, timestamp, duration, status, and affected files where relevant.
- Never present hidden model chain-of-thought. Show concise plans, action rationales, evidence, and observable results.
- Terminal output is sanitized, virtualized, searchable, selectable, and capped in live memory.
- New activity does not force-scroll when the user is reviewing older events; show a “Jump to latest” control.

### Session Controls

- Primary control changes by state: Start, Pause, Resume, Review, or Create pull request.
- Stop/cancel is visually separate and requires impact confirmation.
- Unavailable provider capabilities are hidden or disabled with explanation.
- Every command shows pending, accepted, and completed feedback.
- Duplicate clicks cannot submit duplicate control actions.

### Approval Experience

Approval cards show:

- Exact proposed action
- Why the agent says it is needed
- Risk level and policy rule
- Affected repository, branch, files, network destination, or service
- Normalized parameters and command preview
- Expiration countdown
- Approve and reject controls

High-risk approvals require a confirmation step. Destructive operations are disabled by default and use explicit typed confirmation if an administrator enables them.

### Diff Review

- File tree groups added, modified, renamed, and deleted files.
- Users can switch between unified and side-by-side views.
- Whitespace changes can be hidden.
- Inline comments create revision requests linked to file and line context.
- Large and binary files show safe summaries instead of attempting full rendering.
- Verification status remains visible while reviewing.

## Page Patterns

### Home

- Recent sessions and tasks
- Sessions requiring the current user's approval
- Active agent status
- Usage summary
- Repository health or integration warnings
- First-run checklist for new workspaces

### Tasks

- Table and board views
- Filters for repository, state, agent, creator, and assignee
- Create task with an advanced section for provider, model profile, branch, policy, limits, and context
- Task detail combines requirements, sessions, pull requests, and history

### Sessions

- Active, waiting, review-ready, completed, and failed filters
- Live status, participant presence, duration, cost, and changed-file count
- Clear recovery actions for failed or interrupted sessions

### Repositories

- Connected repositories, access state, default branch, last sync, and GitHub App installation
- Permission and webhook health checks
- Repository-specific policy and allowed command configuration

### Agents

- Provider-neutral agent profiles
- Provider/model, instruction summary, capabilities, policy, and version history
- Editing creates a new immutable agent version for future sessions

### Settings

- General workspace identity
- Members and roles
- Integrations
- Security and approval policy
- Usage limits
- Billing
- Audit log

## Empty, Loading, and Error States

### Empty States

- Explain the value and the single next action.
- Use small, restrained illustrations only on onboarding and top-level empty pages.
- Never show an empty table with no guidance.

### Loading States

- Use skeletons matching final geometry for page and panel loading.
- Use inline progress for control commands.
- Runner provisioning shows named stages: validating, creating branch, preparing environment, starting agent.
- Do not use indefinite spinners when progress or timeout information is available.

### Error States

- State what failed in plain language.
- Explain whether work and history are safe.
- Offer a specific retry, reconnect, configuration, or support action.
- Include a copyable request/session reference without exposing internals.
- Preserve user input after recoverable errors.

### Offline and Reconnect

- Show connection state in the session header.
- Queue no security-sensitive mutations while offline.
- On reconnect, replay events from the last acknowledged sequence before enabling controls.
- If permissions changed while disconnected, explain the revoked capability.

## Icons

Use Lucide React. Use stroke-based icons with consistent 1.75–2 px visual weight.

- 16 px for compact inline actions
- 18 px for default buttons and navigation
- 20 px for prominent controls
- 24 px for empty-state or status illustrations

Every icon-only button needs an accessible name and tooltip. Do not mix unrelated icon families.

## Motion

- Standard transition: 120–180 ms with ease-out
- Panel and drawer transition: 180–240 ms
- Presence changes may fade subtly; avoid bouncing or pulsing avatars
- Running state uses a restrained progress indicator, not continuous decorative animation
- Respect `prefers-reduced-motion` and remove non-essential transitions

## Responsive Behavior

- Supported production minimum: 1024 px desktop width for full session controls
- 768–1023 px: condensed navigation, collapsible side panels, inspector drawer
- Below 768 px: monitoring, approvals, messages, and basic controls remain usable; full diff review may recommend desktop
- Marketing and authentication pages are fully mobile responsive

## Accessibility

- WCAG 2.2 AA is the baseline.
- Maintain logical heading and landmark structure.
- Keyboard users can enter and exit terminal, diff, and resizable panels without traps.
- Provide a skip link to session content.
- Announce approval requests and terminal state changes through polite live regions.
- Do not announce every streamed output line.
- Focus moves to the relevant summary after major state transitions only when initiated by the user.
- Verify the application at 200% browser zoom and with reduced motion.

## Content Style

- Use concise, direct language.
- Prefer “Start session” over “Initiate agentic execution.”
- Prefer “Waiting for approval” over “Human-in-the-loop required.”
- State consequences: “This will stop the runner and preserve current changes.”
- Avoid pretending the agent is human or certain.
- Distinguish “agent reported,” “Weave verified,” and “human approved.”

## UI Acceptance Criteria

1. A first-time user can connect a repository and start a session without documentation.
2. A user can identify session status, current activity, changed files, pending approvals, and next action within five seconds.
3. Every session control is keyboard accessible and provides visible command feedback.
4. Reconnecting produces no duplicate events and visibly restores the authoritative state.
5. Long timelines, logs, and diffs remain responsive through virtualization and pagination.
6. Dark and light themes pass automated contrast checks and manual review.
7. The flagship flow passes keyboard-only, screen-reader, 200% zoom, and reduced-motion testing.

