import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, spyOn, userEvent, waitFor, within } from "storybook/test";
import { API } from "#/api/api";
import {
	MockHibernatedWorkspace,
	MockSleepNoticeWorkspace,
	MockStoppedWorkspace,
	MockWorkspace,
	MockWorkspaceAgentDisconnected,
	MockWorkspaceCooplay,
	mockApiError,
} from "#/testHelpers/entities";
import { withToaster } from "#/testHelpers/storybook";
import { WorkspacePowerAlert } from "./WorkspacePowerAlert";

const hibernatedPower = MockHibernatedWorkspace.cooplay?.power;

const meta: Meta<typeof WorkspacePowerAlert> = {
	title: "pages/WorkspacePage/WorkspacePowerAlert",
	component: WorkspacePowerAlert,
	args: { workspace: MockHibernatedWorkspace },
	decorators: [withToaster],
};

export default meta;
type Story = StoryObj<typeof WorkspacePowerAlert>;

const withPower = (
	state: "paused" | "cold" | "waking" | "sleeping" | "unknown",
) => ({
	...MockHibernatedWorkspace,
	cooplay: {
		...MockWorkspaceCooplay,
		power: { state, since: "2022-05-17T18:00:00Z", reason: "" },
	},
});

export const Hibernated: Story = {
	beforeEach: () => {
		spyOn(API, "wakeWorkspaceAgent").mockResolvedValue({
			result: "requested",
		});
	},
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		const body = within(canvasElement.ownerDocument.body);
		await expect(
			canvas.getByRole("heading", { name: "Workspace hibernated" }),
		).toBeVisible();
		await expect(canvas.getByText(/\(idle\)/)).toBeVisible();
		await userEvent.click(canvas.getByRole("button", { name: "Wake" }));
		await waitFor(() =>
			expect(API.wakeWorkspaceAgent).toHaveBeenCalledWith(
				MockWorkspaceAgentDisconnected.id,
			),
		);
		await body.findByText("Waking the workspace.");
	},
};

export const WakeWhileAwake: Story = {
	beforeEach: () => {
		spyOn(API, "wakeWorkspaceAgent").mockResolvedValue({ result: "awake" });
	},
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		const body = within(canvasElement.ownerDocument.body);
		await userEvent.click(canvas.getByRole("button", { name: "Wake" }));
		await body.findByText("The workspace is already awake.");
	},
};

export const WakeFailed: Story = {
	beforeEach: () => {
		spyOn(API, "wakeWorkspaceAgent").mockRejectedValue(
			mockApiError({ message: "Workspace agent not found." }),
		);
	},
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		const body = within(canvasElement.ownerDocument.body);
		const wakeButton = canvas.getByRole("button", { name: "Wake" });
		await userEvent.click(wakeButton);
		await body.findByText("Workspace agent not found.");
		await waitFor(() => expect(wakeButton).toBeEnabled());
	},
};

export const Paused: Story = {
	args: { workspace: withPower("paused") },
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(
			canvas.getByRole("heading", { name: "Workspace paused" }),
		).toBeVisible();
		await expect(canvas.getByRole("button", { name: "Wake" })).toBeVisible();
	},
};

export const Cold: Story = {
	args: { workspace: withPower("cold") },
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(
			canvas.getByRole("heading", { name: "Workspace is cold" }),
		).toBeVisible();
		await expect(canvas.getByText(/its processes ended/i)).toBeVisible();
	},
};

export const GoingToSleep: Story = {
	args: { workspace: withPower("sleeping") },
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(
			canvas.getByRole("heading", { name: "Workspace is going to sleep" }),
		).toBeVisible();
		await expect(canvas.getByRole("button", { name: "Wake" })).toBeVisible();
	},
};

export const Waking: Story = {
	args: { workspace: withPower("waking") },
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(
			canvas.getByRole("heading", { name: "Waking the workspace" }),
		).toBeVisible();
		await expect(
			canvas.queryByRole("button", { name: "Wake" }),
		).not.toBeInTheDocument();
	},
};

export const Unknown: Story = {
	args: { workspace: withPower("unknown") },
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(
			canvas.getByRole("heading", { name: "Workspace power state unknown" }),
		).toBeVisible();
		await expect(canvas.queryByText(/unhealthy/i)).not.toBeInTheDocument();
	},
};

export const SleepNotice: Story = {
	args: { workspace: MockSleepNoticeWorkspace },
	beforeEach: () => {
		spyOn(API, "keepWorkspaceAwake").mockResolvedValue({
			requested_at: "2022-05-17T18:00:30Z",
			for_seconds: 7200,
		});
	},
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		const body = within(canvasElement.ownerDocument.body);
		await expect(
			canvas.getByRole("heading", { name: "Workspace goes to sleep soon" }),
		).toBeVisible();
		await userEvent.click(
			canvas.getByRole("button", { name: "Keep awake 2 h" }),
		);
		await waitFor(() =>
			expect(API.keepWorkspaceAwake).toHaveBeenCalledWith(
				MockSleepNoticeWorkspace.id,
				{ for_seconds: 7200 },
			),
		);
		await body.findByText(
			"Asked the platform to keep the workspace awake for 2 hours.",
		);
	},
};

export const SleepNoticeKeepAwakeFailed: Story = {
	args: { workspace: MockSleepNoticeWorkspace },
	beforeEach: () => {
		spyOn(API, "keepWorkspaceAwake").mockRejectedValue(
			mockApiError({ message: "Workspace template is not managed." }),
		);
	},
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		const body = within(canvasElement.ownerDocument.body);
		await userEvent.click(
			canvas.getByRole("button", { name: "Keep awake 2 h" }),
		);
		await body.findByText("Workspace template is not managed.");
	},
};

export const KeptAwake: Story = {
	args: {
		workspace: {
			...MockWorkspace,
			cooplay: {
				...MockWorkspaceCooplay,
				keepalive_until: "2022-05-17T20:00:00Z",
			},
		},
	},
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(canvas.getByText(/kept awake until/i)).toBeVisible();
		await expect(canvas.queryByRole("button")).not.toBeInTheDocument();
	},
};

export const Running: Story = {
	args: { workspace: { ...MockWorkspace, cooplay: MockWorkspaceCooplay } },
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(canvas.queryByRole("heading")).not.toBeInTheDocument();
	},
};

// A stopped workspace shows its build status, even if the platform has not
// cleared the power state yet.
export const StoppedWithPowerState: Story = {
	args: {
		workspace: {
			...MockStoppedWorkspace,
			cooplay: { ...MockWorkspaceCooplay, power: hibernatedPower },
		},
	},
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(canvas.queryByRole("heading")).not.toBeInTheDocument();
	},
};
