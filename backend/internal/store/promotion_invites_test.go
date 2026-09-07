package store

import (
	"strings"
	"testing"
	"time"
)

func TestPromotionValidation(t *testing.T) {
	valid := func() PromotionInvite {
		return PromotionInvite{Name: "Campus", Channel: "club", Code: "club_2026", Tags: []string{" A ", "A"}, ExpiresAt: time.Now().Add(time.Hour), MaxRegistrations: 5, GrantUSD: 2, GrantDays: 30, PlanDays: 30,
			Headline: " 开学季 ", DiscountDays: 30, TopupBonusDays: 30, UsageDiscountPercent: 20, TopupBonusPercent: 50, MilestoneSessionUSD: 1}
	}
	p := valid()
	if err := validatePromotion(&p); err != nil {
		t.Fatal(err)
	}
	if p.Code != "CLUB_2026" || len(p.Tags) != 1 || p.Tags[0] != "A" || p.Headline != "开学季" || !p.HasRewards() {
		t.Fatalf("normalization: %+v", p)
	}
	for name, mutate := range map[string]func(*PromotionInvite){
		"expired":  func(p *PromotionInvite) { p.ExpiresAt = time.Now().Add(-time.Hour) },
		"negative": func(p *PromotionInvite) { p.GrantUSD = -1 },
		"days":     func(p *PromotionInvite) { p.PlanDays = 0 },
		"limit":    func(p *PromotionInvite) { p.MaxRegistrations = 0 },
		"code":     func(p *PromotionInvite) { p.Code = "<script>" },
		"free":     func(p *PromotionInvite) { p.PlanCode = "free" },
		"discount": func(p *PromotionInvite) { p.UsageDiscountPercent = 101 },
		"bonus":    func(p *PromotionInvite) { p.TopupBonusPercent = -5 },
		"window":   func(p *PromotionInvite) { p.TopupBonusDays = 4000 },
		"headline": func(p *PromotionInvite) { p.Headline = strings.Repeat("长", 121) },
	} {
		t.Run(name, func(t *testing.T) {
			p := valid()
			mutate(&p)
			if validatePromotion(&p) == nil {
				t.Fatal("invalid offer accepted")
			}
		})
	}
}
