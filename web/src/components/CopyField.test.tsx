import { act, fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { CopyField } from "./CopyField";

// The value copied here is a proposal's pr_url (loam-nvb.13) or a repo
// identifier (loam-nvb.9) -- CopyField is generic, and nothing in it knows
// about SSH keys or CredentialService.
const PR_URL = "https://forge.example/acme/widgets/pulls/42";

/** Replaces navigator.clipboard for one test, restoring it afterwards. */
const withClipboard = (value: Clipboard | undefined): (() => void) => {
  const original = Object.getOwnPropertyDescriptor(navigator, "clipboard");
  Object.defineProperty(navigator, "clipboard", { value, configurable: true });
  return () => {
    if (original === undefined) {
      Reflect.deleteProperty(navigator, "clipboard");
      return;
    }
    Object.defineProperty(navigator, "clipboard", original);
  };
};

describe("CopyField", () => {
  it("labels the value and exposes it as a read-only, still-selectable field", () => {
    render(<CopyField label="Pull request URL" value={PR_URL} />);
    const field = screen.getByLabelText("Pull request URL");
    expect(field).toHaveValue(PR_URL);
    // readOnly, not disabled: a disabled input cannot be focused or
    // selected, which removes the keyboard fallback for copying.
    expect(field).toHaveAttribute("readonly");
    expect(field).toBeEnabled();
  });

  it("names the copy button after the field, so two of them are distinguishable", () => {
    render(
      <>
        <CopyField label="Pull request URL" value={PR_URL} />
        <CopyField label="Upstream branch" value="loam/work-42" />
      </>,
    );
    expect(screen.getByRole("button", { name: "Copy Pull request URL" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Copy Upstream branch" })).toBeInTheDocument();
  });

  it("writes the value to the clipboard and confirms it in a live region", async () => {
    // userEvent.setup() installs a clipboard stub and restores it on cleanup.
    const user = userEvent.setup();
    render(<CopyField label="Pull request URL" value={PR_URL} />);
    expect(screen.getByRole("status")).toHaveTextContent("");
    await user.click(screen.getByRole("button", { name: "Copy Pull request URL" }));
    await expect(navigator.clipboard.readText()).resolves.toBe(PR_URL);
    // Announced, not merely recoloured -- a colour change reaches nobody
    // using a screen reader.
    expect(screen.getByRole("status")).toHaveTextContent("Copied Pull request URL");
  });

  // The three cases below drive the button with fireEvent rather than
  // userEvent on purpose: userEvent.setup() installs its own clipboard stub
  // over navigator.clipboard, which is precisely the thing under test here.

  it("says so, in the same live region, when there is no Clipboard API at all", async () => {
    // No secure context, no navigator.clipboard. The user has to fall back
    // to selecting the field, so the message says that.
    const restore = withClipboard(undefined);
    try {
      render(<CopyField label="Pull request URL" value={PR_URL} />);
      fireEvent.click(screen.getByRole("button", { name: "Copy Pull request URL" }));
      expect(await screen.findByText(/Could not copy Pull request URL/)).toHaveTextContent(
        "Could not copy Pull request URL. Select the value and copy it manually.",
      );
    } finally {
      restore();
    }
  });

  it("says so when the clipboard write is rejected", async () => {
    const writeText = vi.fn<(text: string) => Promise<void>>(() =>
      Promise.reject(new Error("permission denied")),
    );
    const restore = withClipboard({ writeText } as unknown as Clipboard);
    try {
      render(<CopyField label="Pull request URL" value={PR_URL} />);
      fireEvent.click(screen.getByRole("button", { name: "Copy Pull request URL" }));
      expect(await screen.findByText(/Could not copy Pull request URL/)).toBeInTheDocument();
      expect(writeText).toHaveBeenCalledWith(PR_URL);
    } finally {
      restore();
    }
  });

  it("clears the confirmation after it has been up long enough to read", async () => {
    const restore = withClipboard({
      writeText: () => Promise.resolve(),
    } as unknown as Clipboard);
    vi.useFakeTimers();
    try {
      render(<CopyField label="Pull request URL" value={PR_URL} />);
      fireEvent.click(screen.getByRole("button", { name: "Copy Pull request URL" }));
      await act(async () => {
        await Promise.resolve();
      });
      expect(screen.getByRole("status")).toHaveTextContent("Copied Pull request URL");
      await act(async () => {
        await vi.advanceTimersByTimeAsync(2000);
      });
      expect(screen.getByRole("status")).toHaveTextContent("");
    } finally {
      vi.useRealTimers();
      restore();
    }
  });
});
