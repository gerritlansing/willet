# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub:
[**Report a vulnerability**](https://github.com/gerritlansing/willet/security/advisories/new)
(the repository's **Security** tab → **Report a vulnerability**).

Don't open a public issue, pull request or discussion for a security problem.

Please include:

- what the problem is and what an attacker could do with it;
- the steps to reproduce it, and the configuration involved (with secrets removed);
- the willet version (`willet --version`) you tested, and your microsandbox (`msb --version`) and host OS versions.

## What to expect

This is a small project maintained in spare time, so these are aims, not guarantees:

- an acknowledgement within 7 days;
- an assessment, and a fix or mitigation plan, as soon as practical after that;
- a published GitHub security advisory once a fix is available, crediting you unless you'd rather stay anonymous.

Please give us a reasonable chance to fix the problem before disclosing it publicly.

## Supported versions

Only the latest release is supported. Security fixes are made in a new release, not backported to older ones.

## Scope

Report here problems in how willet itself works, such as mishandled credentials or microsandbox configured less securely than intended. Report vulnerabilities in the components it relies on to those projects:

- VM isolation and network policy enforcement: [microsandbox](https://github.com/superradcompany/microsandbox/security)
- the scale set client: [actions/scaleset](https://github.com/actions/scaleset/security)
- the GitHub Actions runner, and GitHub itself: [GitHub's bug bounty program](https://bounty.github.com/)

If you're not sure where something belongs, report it here and we'll help route it.
