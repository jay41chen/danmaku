package model

import "testing"

// 逐列對應 ROADMAP 的規則：期望值用手寫的表，不是從實作推導
func TestDecideRoleChange(t *testing.T) {
	const (
		owner  = RoomRoleOwner
		mod    = RoomRoleModerator
		banned = RoomRoleBanned
		viewer = RoomRoleViewer
	)
	tests := []struct {
		name                   string
		actor, target, desired RoomRole
		bannedByOwner          bool
		want                   RoleDecision
	}{
		// 一般觀眾和被 ban 的人什麼都不能做；被 ban 的人和一般觀眾結果要相同，shadow ban 才藏得住
		{"viewer bans viewer", viewer, viewer, banned, false, RoleChangeForbidden},
		{"viewer removes banned", viewer, banned, viewer, false, RoleChangeForbidden},
		{"viewer promotes", viewer, viewer, mod, false, RoleChangeForbidden},
		{"viewer targets owner", viewer, owner, banned, false, RoleChangeForbidden},
		{"banned bans viewer", banned, viewer, banned, false, RoleChangeForbidden},
		{"banned removes banned", banned, banned, viewer, false, RoleChangeForbidden},
		{"banned promotes", banned, viewer, mod, false, RoleChangeForbidden},
		{"banned targets owner", banned, owner, banned, false, RoleChangeForbidden},

		// 房管
		{"mod bans viewer", mod, viewer, banned, false, RoleChangeAllow},
		{"mod re-bans mod's ban", mod, banned, banned, false, RoleChangeNoop},
		{"mod re-bans owner's ban keeps assigned_by", mod, banned, banned, true, RoleChangeNoop},
		{"mod removes mod's ban", mod, banned, viewer, false, RoleChangeAllow},
		{"mod removes owner's ban", mod, banned, viewer, true, RoleChangeTargetForbidden},
		{"mod removes viewer", mod, viewer, viewer, false, RoleChangeNoop},
		{"mod bans mod", mod, mod, banned, false, RoleChangeTargetForbidden},
		{"mod removes mod", mod, mod, viewer, false, RoleChangeTargetForbidden},
		{"mod bans owner", mod, owner, banned, false, RoleChangeTargetForbidden},
		{"mod removes owner", mod, owner, viewer, false, RoleChangeTargetForbidden},
		{"mod promotes viewer", mod, viewer, mod, false, RoleChangeForbidden},
		{"mod promotes mod", mod, mod, mod, false, RoleChangeForbidden},
		{"mod promotes banned", mod, banned, mod, false, RoleChangeForbidden},
		{"mod promotes owner", mod, owner, mod, false, RoleChangeTargetForbidden},

		// 房主
		{"owner bans viewer", owner, viewer, banned, false, RoleChangeAllow},
		{"owner bans mod", owner, mod, banned, false, RoleChangeAllow},
		{"owner re-bans mod's ban takes over", owner, banned, banned, false, RoleChangeAllow},
		{"owner re-bans own ban", owner, banned, banned, true, RoleChangeAllow},
		{"owner promotes viewer", owner, viewer, mod, false, RoleChangeAllow},
		{"owner promotes mod", owner, mod, mod, false, RoleChangeNoop},
		{"owner promotes banned", owner, banned, mod, false, RoleChangeConflict},
		{"owner removes mod", owner, mod, viewer, false, RoleChangeAllow},
		{"owner removes mod's ban", owner, banned, viewer, false, RoleChangeAllow},
		{"owner removes own ban", owner, banned, viewer, true, RoleChangeAllow},
		{"owner removes viewer", owner, viewer, viewer, false, RoleChangeNoop},
		{"owner bans self", owner, owner, banned, false, RoleChangeTargetForbidden},
		{"owner promotes self", owner, owner, mod, false, RoleChangeTargetForbidden},
		{"owner removes self", owner, owner, viewer, false, RoleChangeTargetForbidden},

		// fail closed：沒預期到的值不能被放行
		{"unknown actor", RoomRole("admin"), viewer, banned, false, RoleChangeForbidden},
		{"empty actor", RoomRole(""), viewer, banned, false, RoleChangeForbidden},
		{"desired owner", owner, viewer, owner, false, RoleChangeForbidden},
		{"desired empty", owner, viewer, RoomRole(""), false, RoleChangeForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DecideRoleChange(tt.actor, tt.target, tt.desired, tt.bannedByOwner)
			if got != tt.want {
				t.Errorf("DecideRoleChange(%s, %s, %s, %v) = %d, want %d", tt.actor, tt.target, tt.desired, tt.bannedByOwner, got, tt.want)
			}
		})
	}
}
