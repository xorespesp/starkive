# starkive

A private archive for the GitHub repositories you want to remember. Save repositories without starring them, sort them into lists, write notes, and keep them even after they disappear from GitHub.

![starkive dashboard preview](docs/preview.png)

## Why

GitHub stars are a public bookmark list:

- Your stars and star lists are visible to everyone.
- There is nowhere to write notes about a repository.
- Unstar a repository, or have it deleted or made private, and it is gone from your list without a trace.

Keeping a private list in a notes app instead means copying links by hand, writing descriptions yourself, and finding out too late that a link is dead. starkive does that bookkeeping for you, in a repository you own.

starkive isn't a smart star organizer with AI tags and semantic search. It's a quiet, private record of the repositories you care about, and it stays.

## What you get

- **Save without starring.** Add any repository from the dashboard, or from any GitHub repository page with the **Add to starkive** bookmarklet. Nothing shows up on your public profile.
- **Entries outlive the repository.** starkive keeps each repository's description, language, topics and license. When a repository is deleted or made private, its entry stays readable and is marked as unavailable, so you still know what it was. Renames are followed, and unstarring on GitHub doesn't remove anything.
- **Yours, in your own repository.** The archive is plain JSON in a repository on your GitHub account (private by default), and every change is a commit, so you have the full history and can take it anywhere. Lists and notes are yours alone, and only your GitHub account can sign in.

And the rest of what you'd expect:

- **Bring your stars along.** Import the stars you already have, with their GitHub star lists, and let starkive pick up new stars automatically (every day by default).
- **One place on every device.** A web dashboard you can open from your laptop or phone, with search, lists and notes.
- **No server to run, no bill.** It runs on Vercel's free plan and GitHub, with no database to host or maintain.
- **Read-only toward your stars.** starkive never stars or unstars anything. Its token can write only to your archive repository, and otherwise just reads your stars.

## Quickstart

### Prerequisites

- A GitHub account
- A [Vercel](https://vercel.com) account connected to that GitHub account (the free plan is enough). Vercel creates the dashboard's code repository in your GitHub account, so sign up with GitHub or connect GitHub in Vercel first.
- [Git](https://git-scm.com/)
- [Go](https://go.dev/dl/) 1.22 or later

### Steps

1. **Clone this repository.**

   ```
   git clone https://github.com/xorespesp/starkive
   ```

> [!NOTE]
> Use `git clone`, not **Download ZIP**. The setup tool depends on your local Git clone.

2. **Run the setup tool** in your clone:

   ```
   go run ./tools/setup
   ```

   It opens a setup page in your browser and walks you through the steps that need you:
   - **Create archive repository.** Enter a name for your archive (your GitHub username works well), then click **Create the repository**. The name and **Private** are filled in: keep the name and create it empty, with no README, `.gitignore` or license. Then click **Create the token**. That page is filled in for your archive repository: under **Repository access**, choose **Only select repositories**, pick that repository, generate the token, and paste it back.
   - **Deploy dashboard.** Click **Create the Vercel project** and pick your Git account (Vercel puts a copy of the dashboard's code there). Keep the repository name, paste the value the setup page shows under **Environment Variables**, and click **Deploy**. The setup page notices when the deployment is ready and continues on its own. If Vercel gives the dashboard a different address, enter it there (the production domain under the Vercel project's **Domains**).
   - **Create sign-in app.** Click **Create the GitHub App**, then **Create GitHub App** on GitHub's page. This app is how you sign in, and it gets no access to your repositories.

> [!NOTE]
> To run the setup headless, use `go run ./tools/setup -headless`.

3. **Open the dashboard and sign in with GitHub.** Your archive picks up new stars automatically (every day by default). Change how often, turn it off, or sync right away in **Settings**.

If you ever replace the token, run the setup tool again, put the new value in Vercel, and redeploy. The tool tells you if anything else needs to change.

## Updating

Updating the dashboard isn't automated yet; an easy way to bring new versions into your dashboard's code repository is planned. Updates never touch your archive, and if a new version can't use your existing archive, the dashboard says so instead of changing anything.

## Development

```
go test ./...
go run ./tools/dev-server    # http://localhost:3000, reads .env.local
```

The dev server writes to the repository in `.env.local`, so use a separate empty repository for it:

1. Run the setup tool with that repository. In **Deploy dashboard**, skip the Vercel project and put the value it shows in `.env.local` as `STARKIVE_DB_REPO_ACCESS_KEY=<value>`.
2. Start the dev server, then click **Or enter the deployed dashboard's Vercel domain manually** on the setup page (with `-headless`, type it at the prompt) and enter `http://localhost:3000`.
3. After the setup, turn off auto sync in that dashboard's **Settings**.
