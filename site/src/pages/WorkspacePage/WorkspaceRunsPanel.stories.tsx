import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, within } from "storybook/test";
import {
	MockWorkspaceCooplay,
	MockWorkspaceCooplayRun,
} from "#/testHelpers/entities";
import { WorkspaceRunsPanel } from "./WorkspaceRunsPanel";

const meta: Meta<typeof WorkspaceRunsPanel> = {
	title: "pages/WorkspacePage/WorkspaceRunsPanel",
	component: WorkspaceRunsPanel,
	args: { cooplay: MockWorkspaceCooplay },
};

export default meta;
type Story = StoryObj<typeof WorkspaceRunsPanel>;

export const Default: Story = {
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		const table = canvas.getByRole("table", { name: "Runs" });
		const rows = within(table).getAllByRole("row");
		await expect(rows).toHaveLength(3);
		await expect(rows[1]).toHaveTextContent(MockWorkspaceCooplayRun.id);
		await expect(rows[1]).toHaveTextContent("Waiting for you");
		await expect(rows[1]).toHaveTextContent(
			`Grant ${MockWorkspaceCooplayRun.grant_id}`,
		);
		await expect(rows[2]).toHaveTextContent("Done");
		await expect(rows[2]).toHaveTextContent("Owner");
		await expect(
			canvas.getByText(
				/created by run r-01jbqs5x7kx0 of claude-code under grant g-01jbqs4a2m8n \(expires/i,
			),
		).toBeVisible();
		await expect(canvas.getByText("go test ./...")).toBeVisible();
		await expect(canvas.getByText(/exited with code 1/i)).toBeVisible();
		await expect(canvas.getByText(/3 uncommitted files/)).toBeVisible();
		await expect(canvas.getByRole("link", { name: "#7" })).toHaveAttribute(
			"href",
			MockWorkspaceCooplay.pr?.url,
		);
	},
};

export const Empty: Story = {
	args: {
		cooplay: { runs: [] },
	},
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(
			canvas.getByText("No agent Runs in this workspace."),
		).toBeVisible();
		await expect(canvas.queryByRole("table")).not.toBeInTheDocument();
		await expect(canvas.queryByText(/created by/i)).not.toBeInTheDocument();
	},
};

export const RevokedGrant: Story = {
	args: {
		cooplay: {
			...MockWorkspaceCooplay,
			source: MockWorkspaceCooplay.source && {
				...MockWorkspaceCooplay.source,
				grant_revoked_at: "2022-05-17T18:30:00Z",
			},
		},
	},
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(canvas.getByText(/\(revoked\)/)).toBeVisible();
	},
};

export const CreatedByOwner: Story = {
	args: {
		cooplay: {
			...MockWorkspaceCooplay,
			source: MockWorkspaceCooplay.source && {
				...MockWorkspaceCooplay.source,
				kind: "employee",
				grant_id: "",
				run_id: "",
				grant_expires_at: undefined,
			},
		},
	},
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(canvas.queryByText(/created by/i)).not.toBeInTheDocument();
	},
};

// Pull request facts come from inside the workspace, so a URL that is not
// a web URL is shown as text.
export const UntrustedPullRequestURL: Story = {
	args: {
		cooplay: {
			...MockWorkspaceCooplay,
			pr: MockWorkspaceCooplay.pr && {
				...MockWorkspaceCooplay.pr,
				url: "javascript:alert(1)",
			},
		},
	},
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(canvas.getByText(/^#7 open/)).toBeVisible();
		await expect(canvas.queryByRole("link")).not.toBeInTheDocument();
	},
};
