# Scruple

Glossary. No implementation details.

- **Org**: the owner of repos, rules, members, tokens and billing. Every
  account has a **personal org**, and a user can belong to many orgs.
- **Rule**: a plain-language question about code, with a threshold, a severity
  and optional paths. It belongs to a repo or to an org.
- **Org rule**: a rule defined once for an org and applied to its repos.
- **Rule version**: one immutable revision of a rule. Edits create new
  versions.
- **Override**: a repo's tuning of an org rule (severity, threshold, paths).
  An override can't turn the rule off.
- **Locked rule**: an org rule that repos can't override.
- **Shadow mode**: a rule runs and records results but doesn't block or
  comment.
- **Enforce**: switching a rule from shadow mode to blocking.
- **Baseline**: the violations that already existed when a rule was enforced.
  They don't fail PRs.
- **Pilot repos**: the first repos an org rule is rolled out to.
- **Hit**: code a rule matched above its threshold, in any mode.
- **Violation**: a hit from an enforced rule that isn't baselined or
  dismissed. Violations fail the PR.
- **Near miss**: one of the closest matches below a rule's threshold.
- **Project**: a top-level section of a repo. It is the main unit users
  work in, and it has its own dashboard.
- **Section**: a named part of a repo, covering a set of paths. Sections nest
  to any depth. Rules and admins attach to sections and apply to everything
  below them.
- **Inheritance**: rules flow down org → repo → section → child section. A
  lower level can override an inherited rule but can't remove it.
