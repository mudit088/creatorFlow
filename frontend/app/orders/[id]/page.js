import OrderDownloads from "@/components/OrderDownloads";

export const metadata = {
  title: "Your purchase",
  // A download page has nothing to gain from being indexed and something to
  // lose: the order id is the capability that unlocks it.
  robots: { index: false, follow: false },
};

export default async function OrderPage({ params, searchParams }) {
  const { id } = await params;
  const { email } = await searchParams;

  return (
    <main className="mx-auto w-full max-w-lg px-5 pb-24 pt-16">
      <OrderDownloads orderId={id} initialEmail={email || ""} />
    </main>
  );
}
