## Summary

<!-- Describe the user-visible change and why it is needed. -->

## Linked issue or task

<!-- Link the issue or name the task, for example: Fixes #123 or GAP-085. -->

## Scope

<!-- List the files or components changed. Call out intentionally excluded work. -->

## Verification

<!-- Record the commands you ran and their results. Explain any command not run. -->

| Command | Result |
| --- | --- |
|  |  |

## Documentation and compatibility impact

<!-- Note documentation, API, wire-protocol, configuration, migration, or compatibility changes. Write "None" when there is no impact. -->

## Repository gates

- [ ] `make test` passes.
- [ ] `make lint` passes.
- [ ] Shell/YAML checks pass: `make shell-yaml-check`.
- [ ] Make/Docker checks pass: `make make-docker-check`.
- [ ] `make gofmt-check` passes.
- [ ] My commit includes `Co-authored-by: Alexis Okuwa <wojonstech@gmail.com>`.

<!-- If a gate is unavailable or not applicable, explain why in Verification. -->
