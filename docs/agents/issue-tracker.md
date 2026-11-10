# Issue tracker: Local Markdown

Issues and specs for this repo live as markdown files in `features-specs/`.

This repo has no git remote, so there is no GitHub or GitLab issue tracker to
call. `features-specs/` was chosen over the template's default `.scratch/`
because it already holds documents of exactly this kind, and splitting work
docs across two directories buys nothing.

Three files already sit at the top level of `features-specs/` rather than in a
feature directory: `build-auth-service.md` (the auth spec), `auth-fix-plan.md`
and `auth-fix-list.md`. Leave them where they are. The conventions below apply
to new work.

## Conventions

- One feature per directory: `features-specs/<feature-slug>/`
- The spec is `features-specs/<feature-slug>/spec.md`
- Implementation issues are one file per ticket at `features-specs/<feature-slug>/issues/<NN>-<slug>.md`, numbered from `01`, never a single combined tickets file
- Triage state is recorded as a `Status:` line near the top of each issue file (see `triage-labels.md` for the role strings)
- Comments and conversation history append to the bottom of the file under a `## Comments` heading

Files here are committed, not scratch. A link out of one points at the tree
with a relative path, as `auth-fix-list.md` does, so it keeps working from a
file viewer.

## When a skill says "publish to the issue tracker"

Create a new file under `features-specs/<feature-slug>/` (creating the
directory if needed).

## When a skill says "fetch the relevant ticket"

Read the file at the referenced path. The user will normally pass the path or
the issue number directly.

## Wayfinding operations

Used by `/wayfinder`. The **map** is a file with one **child** file per ticket.

- **Map**: `features-specs/<effort>/map.md` (the Notes / Decisions-so-far / Fog body).
- **Child ticket**: `features-specs/<effort>/issues/NN-<slug>.md`, numbered from `01`, with the question in the body. A `Type:` line records the ticket type (`research`/`prototype`/`grilling`/`task`); a `Status:` line records `claimed`/`resolved`.
- **Blocking**: a `Blocked by: NN, NN` line near the top. A ticket is unblocked when every file it lists is `resolved`.
- **Frontier**: scan `features-specs/<effort>/issues/` for files that are open, unblocked, and unclaimed; first by number wins.
- **Claim**: set `Status: claimed` and save before any work.
- **Resolve**: append the answer under an `## Answer` heading, set `Status: resolved`, then append a context pointer (gist + link) to the map's Decisions-so-far in `map.md`.
