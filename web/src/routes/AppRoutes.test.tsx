import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";
import { AppLayout } from "./AppLayout";
import { AppRoutes } from "./AppRoutes";
import { proposalDetailPath, repoDetailPath } from "./paths";

/**
 * The route table is the one part of the shell that can be quietly, visibly
 * wrong: a pattern that never matches renders an empty `<main>`, and a
 * missing `<Switch>` renders two screens at once. Every case below therefore
 * asserts on what the user ends up looking at, at a real URL.
 */

/** Mounts the shell at `path`, and hands back the recording location hook. */
const renderAt = (path: string) => {
  const location = memoryLocation({ path, record: true });
  render(
    <Router hook={location.hook}>
      <AppLayout>
        <AppRoutes />
      </AppLayout>
    </Router>,
  );
  return location;
};

/** The `<h1>` the routed screen rendered. */
const heading = (): string => screen.getByRole("heading", { level: 1 }).textContent ?? "";

describe("the route table", () => {
  it.each([
    ["/", "Repos"],
    ["/credentials", "Credentials"],
    ["/roles", "Roles"],
    ["/proposals", "Proposals"],
    ["/jobs", "Jobs"],
  ])("resolves %s to the %s screen", (path, title) => {
    renderAt(path);
    expect(heading()).toBe(title);
  });

  it("renders exactly one screen, not the fallback alongside the match", () => {
    renderAt("/jobs");
    // Without <Switch>, the path-less fallback <Route> matches every location
    // and renders under the real screen. Two <h1>s is what that looks like.
    expect(screen.getAllByRole("heading", { level: 1 })).toHaveLength(1);
  });
});

describe("the two-segment repo identifier", () => {
  it("rejoins /repos/:group/:name into the wire form the API uses", () => {
    // An enrolled repo is "<group>/<repo_name>", so its URL spans two
    // segments and a single-segment :repo parameter cannot capture it.
    renderAt("/repos/acme/widgets");
    expect(heading()).toBe("acme/widgets");
  });

  it("round-trips a repo identifier through repoDetailPath", () => {
    renderAt(repoDetailPath("acme/widgets"));
    expect(heading()).toBe("acme/widgets");
  });

  it("round-trips a proposal through proposalDetailPath", () => {
    renderAt(proposalDetailPath("acme/widgets", "wb-9c2f1a"));
    expect(heading()).toBe("wb-9c2f1a");
    expect(screen.getByText("acme/widgets")).toBeInTheDocument();
  });

  it("decodes each segment exactly once", () => {
    // wouter runs decodeURI over the location before matching, so the app
    // must NOT decode again — a second decodeURIComponent here would turn
    // "%2520" into a space instead of "%20".
    renderAt("/repos/acme/two%20words");
    expect(heading()).toBe("acme/two words");
  });
});

describe("the not-found fallback", () => {
  it.each([
    ["a URL no screen owns", "/nope"],
    ["a repo URL missing its name segment", "/repos/acme"],
    ["the bare /repos prefix, which is not a screen", "/repos"],
    ["a proposal URL missing its work branch", "/proposals/acme/widgets"],
    ["a repo URL with an extra segment", "/repos/acme/widgets/extra"],
  ])("renders not-found rather than a blank page for %s", (_description, path) => {
    renderAt(path);
    // The server's SPA fallback hands index.html to any non-asset path, so
    // these all reach the client router. A blank <main> is the failure mode.
    expect(heading()).toBe("Not found");
  });

  it("offers a link back that actually navigates", () => {
    const location = renderAt("/nope");
    fireEvent.click(screen.getByRole("link", { name: "Go to Repos" }));
    expect(location.history.at(-1)).toBe("/");
    expect(heading()).toBe("Repos");
  });
});

describe("the nav's current section", () => {
  it("marks the section a detail screen belongs to, not just an exact URL match", () => {
    renderAt("/repos/acme/widgets");
    // wouter's own Link active helper is an exact `currentPath === href`
    // comparison, which would leave every nav entry inactive here.
    expect(screen.getByRole("link", { name: "Repos" })).toHaveAttribute("aria-current", "page");
    expect(screen.getByRole("link", { name: "Proposals" })).not.toHaveAttribute("aria-current");
  });

  it("marks the proposals section on a proposal detail URL", () => {
    renderAt(proposalDetailPath("acme/widgets", "wb-9c2f1a"));
    expect(screen.getByRole("link", { name: "Proposals" })).toHaveAttribute("aria-current", "page");
    expect(screen.getByRole("link", { name: "Repos" })).not.toHaveAttribute("aria-current");
  });

  it("marks nothing current on a URL no section owns", () => {
    // A prefix test would light up Repos here, because "/" prefixes
    // everything and "/repos" prefixes "/repos".
    renderAt("/repos");
    expect(screen.queryByRole("link", { current: "page" })).not.toBeInTheDocument();
  });

  it("links every top-level screen", () => {
    renderAt("/");
    for (const label of ["Repos", "Credentials", "Roles", "Proposals", "Jobs"]) {
      expect(screen.getByRole("link", { name: label })).toBeInTheDocument();
    }
  });
});
