<!-- omit in toc -->
# Contributing to Fireactions

First off, thanks for taking the time to contribute! ❤️

All types of contributions are encouraged and valued. See the [Table of Contents](#table-of-contents) for different ways to help and details about how this project handles them. Please make sure to read the relevant section before making your contribution. It will make it a lot easier for us maintainers and smooth out the experience for all involved. The community looks forward to your contributions. 🎉

> And if you like the project, but just don't have time to contribute, that's fine. There are other easy ways to support the project and show your appreciation, which we would also be very happy about:
>
> - Star the project
> - Tweet about it
> - Mention the project at local meetups and tell your friends/colleagues
> - Refer this project in your project's README

<!-- omit in toc -->
## Table of Contents

- [I Have a Question](#i-have-a-question)
- [I Want To Contribute](#i-want-to-contribute)
  - [Reporting Bugs](#reporting-bugs)
  - [Suggesting Enhancements](#suggesting-enhancements)
- [Release Automation](#release-automation)

## I Have a Question

> Before you ask a question, read the [user guide](docs/user-guide/overview.md).

Search existing [issues](https://github.com/ALameLlama/fireactions/issues) before you open a new one. If an issue covers your question, add a comment there.

If you then still feel the need to ask a question and need clarification, we recommend the following:

- Open an [issue](https://github.com/ALameLlama/fireactions/issues/new).
- Provide as much context as you can about what you're running into.
- Provide project and platform versions (nodejs, npm, etc), depending on what seems relevant.

We will then take care of the issue as soon as possible.

## I Want To Contribute

> ### Legal Notice
>
> When contributing to this project, you must agree that you have authored 100% of the content, that you have the necessary rights to the content and that the content you contribute may be provided under the project license.

### Reporting Bugs

<!-- omit in toc -->
#### Before Submitting a Bug Report

A good bug report shouldn't leave others needing to chase you up for more information. Therefore, we ask you to investigate carefully, collect information and describe the issue in detail in your report. Please complete the following steps in advance to help us fix any potential bug as fast as possible.

- Make sure that you are using the latest version.
- Read the [user guide](docs/user-guide/overview.md) and make sure that your host meets the documented requirements. For support questions, see [I Have a Question](#i-have-a-question).
- Search the [bug tracker](https://github.com/ALameLlama/fireactions/issues?q=label%3Abug) for existing reports.
- Also make sure to search the internet (including Stack Overflow) to see if users outside of the GitHub community have discussed the issue.
- Collect information about the bug:
  - Stack trace (Traceback)
  - OS, Platform and Version (Windows, Linux, macOS, x86, ARM)
  - Version of the interpreter, compiler, SDK, runtime environment, package manager, depending on what seems relevant.
  - Possibly your input and the output
  - Can you reliably reproduce the issue? And can you also reproduce it with older versions?

<!-- omit in toc -->
#### How Do I Submit a Good Bug Report?

> Do not report vulnerabilities or sensitive information in public issues. If private reporting is available, use the [fork's private vulnerability report](https://github.com/ALameLlama/fireactions/security/advisories/new). If it is unavailable, arrange private contact with the fork maintainers before sharing details. Private reporting is not confirmed to be enabled.

We use GitHub issues to track bugs and errors. If you run into an issue with the project:

- Open an [issue](https://github.com/ALameLlama/fireactions/issues/new).
- Explain the behavior you would expect and the actual behavior.
- Please provide as much context as possible and describe the *reproduction steps* that someone else can follow to recreate the issue on their own. This usually includes your code. For good bug reports you should isolate the problem and create a reduced test case.
- Provide the information you collected in the previous section.

### Suggesting Enhancements

This section guides you through submitting an enhancement suggestion for Fireactions, **including completely new features and minor improvements to existing functionality**. Following these guidelines will help maintainers and the community to understand your suggestion and find related suggestions.

<!-- omit in toc -->
#### Before Submitting an Enhancement

- Make sure that you are using the latest version.
- Read the [user guide](docs/user-guide/overview.md) to see whether existing configuration covers your request.
- Search existing [issues](https://github.com/ALameLlama/fireactions/issues). If the enhancement is already suggested, add a comment instead of opening a new issue.
- Find out whether your idea fits with the scope and aims of the project. It's up to you to make a strong case to convince the project's developers of the merits of this feature. Keep in mind that we want features that will be useful to the majority of our users and not just a small subset. If you're just targeting a minority of users, consider writing an add-on/plugin library.

<!-- omit in toc -->
#### How Do I Submit a Good Enhancement Suggestion?

Enhancement suggestions are tracked as [GitHub issues](https://github.com/ALameLlama/fireactions/issues).

- Use a **clear and descriptive title** for the issue to identify the suggestion.
- Provide a **step-by-step description of the suggested enhancement** in as many details as possible.
- **Describe the current behavior** and **explain which behavior you expected to see instead** and why. At this point you can also tell which alternatives do not work for you.
- **Explain why this enhancement would be useful** to most Fireactions users. You may also want to point out the other projects that solved it better and which could serve as inspiration.

## Release Automation

GitHub hosts this fork's source, CI, and releases. This automation does not provide a GitHub runner backend. Fireactions runs Forgejo jobs.

The release workflow uses a fork-controlled GitHub App to create release pull requests, tags, and releases:

1. Create a GitHub App controlled by the fork maintainers.
2. Give the App repository `Contents: read and write` and `Pull requests: read and write` permissions. GitHub grants `Metadata: read` automatically.
3. Install the App on `ALameLlama/fireactions`.
4. Generate a PEM private key for the App.
5. In the repository's Actions secrets, store the App ID as `RELEASE_APP_ID` and the PEM private key as `RELEASE_APP_PRIVATE_KEY`.

The workflow passes the App token to `release-please`. Tags and releases created with this token trigger downstream GitHub workflows. Do not replace it with `GITHUB_TOKEN`, which suppresses these follow-on runs.

The publishing workflow uses `GITHUB_TOKEN` with `contents: write` and `packages: write`. It publishes GitHub release files and `ghcr.io/alamellama/fireactions`. If the container package already exists, grant `ALameLlama/fireactions` Actions access to that package.
