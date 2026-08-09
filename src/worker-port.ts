import type { BalancerState } from "./state.js";
import type {
  AccountFailure,
  AccountObservation,
  RecordedRoute,
  RefreshAccountResult,
  SelectAccountResult,
  TransportPort,
} from "./transport-port.js";
import type { AccountId, ResponseUsage, SelectAccountInput } from "./domain.js";

class WorkerPort implements TransportPort {
  constructor(private readonly state: DurableObjectStub<BalancerState>) {}

  answered(latencyMs: number): Promise<void> {
    return this.state.answered(latencyMs);
  }

  claimResponseId(accountId: AccountId, responseId: string): Promise<void> {
    return this.state.claimResponseId(accountId, responseId);
  }

  observeAccount(observation: AccountObservation): Promise<void> {
    return this.state.observeAccount(observation);
  }

  recordFailure(failure: AccountFailure): Promise<void> {
    return this.state.recordFailure(failure);
  }

  recordRoute(outcome: RecordedRoute): Promise<void> {
    return this.state.recordRoute(outcome);
  }

  recordUsage(usage: ResponseUsage): Promise<void> {
    return this.state.recordUsage(usage);
  }

  refreshAccount(
    accountId: AccountId,
    rejectedAccessToken: string,
  ): Promise<RefreshAccountResult> {
    return this.state.refreshAccount(accountId, rejectedAccessToken);
  }

  selectAccount(input: SelectAccountInput): Promise<SelectAccountResult> {
    return this.state.selectAccount(input);
  }

  websocketClosed(accountId: AccountId): Promise<void> {
    return this.state.websocketClosed(accountId);
  }

  websocketOpened(accountId: AccountId): Promise<void> {
    return this.state.websocketOpened(accountId);
  }
}

export const createWorkerPort = (
  stub: DurableObjectStub<BalancerState>,
): TransportPort => new WorkerPort(stub);
