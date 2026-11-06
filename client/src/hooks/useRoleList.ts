import { fetchRoles, type RoleEntry } from "../api/roles";
import { usePagedList, type PagedResult } from "./usePagedList";

async function fetchRolePage(pageToken?: string): Promise<PagedResult<RoleEntry>> {
  const page = await fetchRoles(pageToken);
  return { items: page.roles, next: page.next_page_token };
}

const roleKey = (r: RoleEntry) => r.user_id;

// 管理名單分頁載入：enabled 變 true（展開）時載第 1 頁，loadMore 接著載；reload 從第 1 頁重來（細節見 usePagedList）
export function useRoleList(enabled: boolean) {
  const { items, ...rest } = usePagedList(fetchRolePage, roleKey, enabled);
  return { roles: items, ...rest };
}
