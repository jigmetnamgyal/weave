# Weave

## Overview

Weave is a multiplayer workspace where software teams collaborate with existing AI coding agents in real time. Instead of replacing Claude Code, OpenAI Codex, Cursor, or future agent runtimes, Weave provides the shared control layer around them: team presence, durable sessions, context, permissions, live activity, intervention, approvals, code review, audit history, and GitHub delivery. The developer-first MVP serves startup engineering teams and software agencies with 3–20 developers. The long-term platform extends the same human-agent collaboration model to any knowledge-work domain without coupling the core product to software development.

## Product Thesis

AI agents are becoming capable of work that lasts hours or days, touches many systems, and requires multiple human decisions. Current AI interfaces are predominantly private and conversational. Weave treats an agent session as a shared, durable work object that teammates can enter, observe, redirect, approve, hand off, and audit.

**Positioning:** Weave is the shared control room for human and AI work.

**Developer-first promise:** Let your whole engineering team work with the same coding agent—from task to reviewed pull request.

## Target Users



### Initial Ideal Customer Profile

- Software agencies and startup engineering teams with 3–20 developers
- Teams using GitHub and at least one AI coding tool weekly
- Teams shipping web applications, APIs, mobile applications, or internal tools
- Teams that need visibility and review before AI-generated changes reach protected branches
- Teams where developers, technical leads, and project managers share delivery responsibility



### Primary Personas

- **Developer:** launches agents, supplies context, intervenes, reviews diffs, and requests revisions
- **Technical lead:** observes several sessions, controls permissions, approves risk-sensitive actions, and reviews delivery quality
- **Project manager:** tracks task progress and blockers without needing terminal access
- **Workspace administrator:** manages members, repositories, integrations, policy, usage, and billing
- **Viewer/client:** sees approved progress and outputs without controlling the agent



## Goals

1. Enable a team to move from a GitHub issue or written task to a reviewed pull request inside one shared, auditable session.
2. Stream agent activity to authorized collaborators with a perceived update latency below two seconds under normal operating conditions.
3. Require explicit human approval for high-risk actions and maintain an immutable audit trail of instructions, commands, decisions, and code changes.
4. Support at least Claude Code and OpenAI Codex through a versioned provider-adapter contract without coupling product logic to either provider.
5. Recover running sessions after transient web, worker, or orchestration failures without losing accepted instructions or duplicating approved actions.
6. Enforce tenant isolation and role-based permissions at every API and data-access boundary.
7. Deliver a polished desktop-first experience that a new team can understand and use without onboarding assistance.



## Non-Goals for the First Release

- Building a foundation model or proprietary coding model
- Replacing the developer's IDE
- Replacing GitHub issues, pull requests, or source control
- Building a general-purpose project-management suite
- Running autonomous production deployments
- Supporting arbitrary local machines as production runners
- Deep integration with tools that lack a stable, automatable interface
- Multi-agent swarms or agent-to-agent delegation
- Serving non-development departments in the initial product

## Core User Flow

1. A user signs in with GitHub and creates or joins a Weave workspace.
2. The user installs the Weave GitHub App and grants access to selected repositories.
3. The user creates a task or imports a GitHub issue.
4. The user selects a repository, base branch, provider, model profile, and execution policy.
5. Weave validates permissions, budget, repository access, and runner capacity.
6. Weave creates a durable session, an isolated execution environment, and a unique working branch.
7. The chosen coding agent receives the task, repository context, policy, and allowed tools.
8. Authorized teammates enter the room and observe plans, commands, logs, file changes, tests, costs, and status in real time.
9. A teammate can add context, queue an instruction, pause execution, or request a revision.
10. Risk-sensitive actions enter an approval queue and cannot execute until an eligible human approves them.
11. The agent runs checks and produces a reviewable diff and completion summary.
12. A developer requests revisions or approves the result.
13. Weave commits the accepted changes and creates a GitHub pull request using the GitHub App.
14. The session becomes read-only, retains its full timeline, and links to the pull request and generated artifacts.



## Features



### Identity, Workspaces, and Access

- GitHub OAuth authentication
- Multi-tenant workspaces
- Workspace invitation flow
- Roles: owner, admin, developer, viewer
- Repository-scoped access checks
- Session-specific participant and approval permissions
- Session revocation when membership or repository access changes



### GitHub Integration

- GitHub App installation and selected-repository access
- Repository and branch discovery
- GitHub issue import
- Isolated working branch per session
- Commit and pull-request creation
- Signed, idempotent webhook processing
- Protected-branch awareness



### Shared Agent Sessions

- Claude Code and OpenAI Codex provider adapters
- Durable session state machine
- Isolated runner per active session
- Live activity and terminal-safe output stream
- Human instructions with ordered delivery
- Pause, resume, cancel, and retry controls
- Presence and reconnect support
- Session timeline retained after completion



### Review and Approval

- Structured action proposals
- Policy-based risk classification
- Human approval or rejection
- File tree and side-by-side/unified diff review
- Inline review comments and revision requests
- Test, lint, build, and type-check summaries
- Final completion summary and pull-request draft



### Security and Governance

- Least-privilege GitHub credentials
- Secrets redaction and encrypted storage
- Egress-controlled execution environments
- Resource and time limits
- Immutable audit records for security-sensitive events
- Workspace usage and cost limits
- Administrator policy controls



### Product Operations

- Plan and usage enforcement
- Session-level token and compute estimates
- Billing through Stripe
- Structured logging, tracing, metrics, and alerts
- Internal support view for failed and stuck sessions



## Scope



### In Scope for MVP

- Responsive web application optimized for desktop
- GitHub as the only source-control provider
- Hosted Linux runners managed by Weave
- One agent operating within one session
- Claude Code and OpenAI Codex provider adapters
- Public and private GitHub repositories selected through the GitHub App
- Shared live activity, human instructions, and session controls
- Approval gates for configured action classes
- Code diffs, verification results, commits, and pull-request creation
- Workspace roles, tenant isolation, audit logs, quotas, and billing foundations



### Deferred

- Cursor extension and local IDE bridge
- GitLab and Bitbucket
- Self-hosted runner pools
- Enterprise SAML SSO and SCIM directory sync
- Data residency controls and customer-managed encryption keys
- Organization-wide policy packs
- Multi-agent orchestration
- Reusable workflow templates
- Mobile applications
- Non-development workrooms



### Explicitly Out of Scope

- Unattended deployment to production
- Direct write access to protected branches
- Arbitrary inbound network access to runners
- Persistent use of customer personal access tokens
- Browsing unrelated repositories or workspace data
- Exposing raw model-chain-of-thought; Weave displays plans, actions, evidence, and summaries instead



## Product Requirements



### Session Safety

- Every session operates on a non-protected branch created for that session.
- Every external side effect is represented as a typed tool action.
- High-risk and destructive actions require approval; destructive actions are disabled by default.
- Approval is bound to the exact action type, parameters, session version, and expiry time.
- Changing action parameters invalidates prior approval.
- Runner credentials are short-lived and scoped to the session.



### Collaboration

- Multiple authorized users can observe a session simultaneously.
- Human messages are persisted before delivery to the runner.
- Each session event has a monotonic sequence number.
- Reconnecting clients resume from the last acknowledged event.
- Only one control command may transition a session at a time.



### Reliability

- Session orchestration is durable and survives process restarts.
- Commands with side effects use idempotency keys.
- A heartbeat detector identifies disconnected or stuck runners.
- Failed sessions expose a clear reason, recovery action, and retained evidence.
- No session may remain indefinitely in `starting`, `running`, `pausing`, or `stopping`.



## Success Criteria

1. A new team can sign in, connect a repository, create a task, and launch its first session in under ten minutes.
2. Two team members can join the same session and receive ordered activity updates without refreshing the page.
3. A developer can pause the agent, send a new instruction, resume it, and see the instruction reflected in subsequent work.
4. A gated command cannot execute without a valid approval from an authorized workspace member.
5. The agent can modify code, run configured checks, present a diff, accept a revision request, and create a pull request.
6. Refreshing or reconnecting reconstructs the same session state and timeline.
7. Cross-workspace access tests fail for every protected resource type.
8. Runner termination, provider timeout, and GitHub API failure result in recoverable, explicit states rather than silent data loss.
9. Logs and stored events contain no detected credentials in automated secret-scanning tests.
10. Production has backups, health checks, alerts, rollback instructions, and documented incident procedures before paid access is enabled.



## Launch Metrics

- Activation: percentage of new workspaces that complete a first agent session within 24 hours
- First value: median time from signup to reviewable diff
- Collaboration: percentage of sessions with two or more human participants
- Completion: percentage of started sessions that reach review or pull-request creation
- Intervention: percentage of sessions with a human instruction, pause, or approval
- Reliability: session failure rate and recovery rate
- Quality: percentage of created pull requests merged by the customer
- Retention: weekly active workspaces and four-week retained workspaces
- Unit economics: agent-provider cost plus runner cost per completed session

