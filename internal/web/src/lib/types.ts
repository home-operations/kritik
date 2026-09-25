// Mirrors internal/webapi/types.go. Grows as later tasks add pages (repos,
// pulls, reviews, queue, usage, follow-ups); keep it scoped to what the shell
// itself needs today.

export type TenantRole = 'admin' | 'member';
export type TenantManagedBy = 'file' | 'dashboard';

export interface TenantMembership {
  slug: string;
  role: TenantRole;
  managedBy: TenantManagedBy;
}

export interface Account {
  id: string;
  displayName: string;
  email: string;
  avatarUrl: string;
}

export interface Me {
  account: Account;
  operator: boolean;
  tenants: TenantMembership[];
}

export type SignInProviderType = 'oidc' | 'github' | 'forgejo';

export interface SignInProvider {
  name: string;
  type: SignInProviderType;
  displayName: string;
}
