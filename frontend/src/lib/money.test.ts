import { beforeEach, describe, expect, it } from "vite-plus/test";

import { initI18n } from "./i18n/index.js";
import { divideMoney, formatMoney, formatSignedMoney, moneyFromMicrodollars } from "./money.js";

beforeEach(() => {
  localStorage.clear();
  initI18n();
});

describe("formatMoney", () => {
  it("renders ordinary amounts with cents", () => {
    expect(formatMoney(moneyFromMicrodollars(1_234_567))).toBe("$1.23");
    expect(formatMoney(moneyFromMicrodollars(0))).toBe("$0.00");
    expect(formatMoney(moneyFromMicrodollars(-2_500_000))).toBe("-$2.50");
  });

  it("flags sub-cent amounts instead of rounding them to $0.00", () => {
    // Below $0.01 a plain format would print $0.00 and hide real spend.
    expect(formatMoney(moneyFromMicrodollars(1))).toBe("<$0.01");
    expect(formatMoney(moneyFromMicrodollars(9_999))).toBe("<$0.01");
    expect(formatMoney(moneyFromMicrodollars(-5_000))).toBe(">-$0.01");
  });

  it("shows exact cents again at the $0.01 boundary", () => {
    expect(formatMoney(moneyFromMicrodollars(10_000))).toBe("$0.01");
  });

  it("drops cents at $100 and above", () => {
    expect(formatMoney(moneyFromMicrodollars(100_000_000))).toBe("$100");
    expect(formatMoney(moneyFromMicrodollars(1_234_567_890))).toBe("$1,235");
    // Just under the boundary cents are still shown.
    expect(formatMoney(moneyFromMicrodollars(99_999_999))).toBe("$100.00");
  });
});

describe("divideMoney", () => {
  it("divides and rounds to the nearest microdollar", () => {
    expect(divideMoney(moneyFromMicrodollars(10), 4)).toEqual({ microdollars: 3 });
    expect(divideMoney(moneyFromMicrodollars(1_000_000), 3)).toEqual({
      microdollars: 333_333,
    });
  });

  it("returns zero instead of dividing by zero", () => {
    expect(divideMoney(moneyFromMicrodollars(1_000_000), 0)).toEqual({
      microdollars: 0,
    });
  });
});

describe("formatSignedMoney", () => {
  it("prefixes a plus on gains, keeps the plain form otherwise", () => {
    expect(formatSignedMoney(moneyFromMicrodollars(1_230_000))).toBe("+$1.23");
    expect(formatSignedMoney(moneyFromMicrodollars(0))).toBe("$0.00");
    expect(formatSignedMoney(moneyFromMicrodollars(-1_230_000))).toBe("-$1.23");
  });
});
