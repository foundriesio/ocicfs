# Security Policy

We take the security of ocicfs seriously. Because ocicfs runs with elevated
privileges (it performs `mount(2)`, manages a content-addressed store, and
verifies fs-verity / content digests), security reports are handled with care and
under coordinated disclosure.

## Reporting a Vulnerability

**Do not open a public GitHub issue for an undisclosed or urgent vulnerability.**

For a not-yet-released or sensitive issue, report it privately to Qualcomm
Product Security:

- Email: [product-security@qualcomm.com](mailto:product-security@qualcomm.com)
- Web: <https://www.qualcomm.com/company/product-security/report-a-bug>

For a public issue that already has an assigned CVE or is otherwise already
disclosed, you may open a regular
[GitHub issue](../../issues/new). If you have a fix ready, submit it following
the process in [CONTRIBUTING.md](CONTRIBUTING.md).

Please include as much of the following as you can:

- A description of the vulnerability and its impact
- Steps to reproduce (and a proof of concept, if available)
- Affected versions, commit, and kernel version
- Any relevant logs, configuration, or screenshots

## Coordinated Disclosure

We follow a Coordinated Vulnerability Disclosure (CVD) process:

- **Initial Response**: we acknowledge your report within 48 hours.
- **Investigation**: we investigate and keep you updated on progress.
- **Resolution**: we work with you to develop and validate a fix.
- **Disclosure**: once a fix is ready, we disclose the issue and credit the
  reporter (unless anonymity is requested).

## On Homing under composefs

ocicfs is developed with the goal of homing under the
[composefs](https://github.com/composefs) organization. Upon acceptance, security
handling will follow that community's process, and this document will be updated
at that time.
