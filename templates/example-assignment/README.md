# Example assignment: hello

A minimal starter repo showing the file structure that classroom50
expects from an assignment template. `gh student accept` clones from
a template, `gh student submit` fetches a couple of files back from
it on every submit, and `gh teacher download` bulk-clones student
copies.

## Files

- [`hello.c`](hello.c): starter code; replace the TODO with code that prints `hello, world` and a newline.
- [`.gitignore`](.gitignore): optional. `gh student submit` re-fetches it from the template on every submission, so a single edit here propagates to every student's next submit.
- [`.github/`](.github/): optional. Same re-fetch behavior as `.gitignore`. Put non-autograde workflows here (linters, formatters, etc.). The autograde workflow itself does **not** live in templates — `gh student submit` fetches it from the classroom's `autograders/` directory on every submit and overwrites `.github/workflows/autograde.yml`, so anything you put at that path here gets replaced. See the wiki page on [Assignment Templates](https://github.com/foundation50/classroom50/wiki/Assignment-Templates) for the full contract.
- This README: student-facing description of the assignment.

## Using it

Teachers: create a repo from these files in your classroom org and mark it as a template (Settings → "Template repository"). The template must be public so students can read it under the "No permission" base setting; a private template would 404 on `gh student accept`. The only escape hatch is GitHub Enterprise Cloud's "internal" visibility (all enterprise members can read), which Free and Team plans don't expose. The slug you give the repo (e.g. `example-assignment`) is what students pass to `gh student accept`.

Students: after your teacher invites you to the org, run `gh student accept <org> <classroom> example-assignment`. That creates your private copy. Clone it, edit `hello.c`, and run `gh student submit` from inside the clone.
