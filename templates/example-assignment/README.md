# Example assignment: hello

A minimal starter repo showing the file structure that
`classroom50-dev` expects from an assignment template
(`gh student accept` clones from a template, `gh student submit`
fetches files back from it, `gh teacher download` collects student
copies).

## Files

- [`hello.c`](hello.c): starter code; replace the TODO with code that prints `hello, world` and a newline.
- [`.gitignore`](.gitignore): optional template artifact. If present, `gh student submit` re-fetches this from the template at submit time.
- [`.github/`](.github/): optional template artifact. Reusable GitHub Actions workflows can live here; if present, `gh student submit` re-fetches this directory at submit time so any autograding the teacher updates flows back to existing student repos.
- This README: student-facing description of the assignment.

## Using it

Teachers: create a repo from these files and mark it as a template
(in the repo's Settings, tick "Template repository") in the org
you've configured for classroom use. The template must be public so
students can read it under the "No permission" base setting; private
templates would require adding each student as a collaborator. The
only escape hatch is GitHub Enterprise Cloud's "internal" visibility
(all enterprise members can read), which Free and Team plans don't
expose. The slug you give the repo (e.g. `example-assignment`) is
what students pass to `gh student accept`.

Students: after your teacher invites you to the org, run
`gh student accept <org> <classroom> example-assignment`. That
creates your private copy. Clone it, edit `hello.c`, and run
`gh student submit` from inside the clone.
